// Command valmind is the Valmin panel daemon.
//
// Startup sequence: 10 §2 (validation gate) then 12 §9.1 (lease, job sweep,
// reconciliation, resume intents, streams).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/valminhq/valmin/internal/api"
	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv)
	stop()

	if err != nil {
		// The logger may not exist yet: the first gate step is the one that builds it.
		fmt.Fprintf(os.Stderr, "valmind: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	// `↯` The recovery command bypasses the daemon gate entirely — filesystem access to
	// the panel's own config and database is the correct authentication factor for a
	// root-equivalent panel (09 §6), and it has to work even when Docker is unreachable,
	// which is exactly when an admin is likely to be locked out and reaching for it.
	if len(args) > 0 && args[0] == "admin" {
		return runAdmin(ctx, args[1:], getenv)
	}

	cfg, err := config.Load(args, getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	slog.SetDefault(cfg.Log.Logger(os.Stderr))

	d, err := gate(ctx, cfg, getenv)
	if err != nil {
		return err
	}
	defer d.close(ctx)

	return d.serve(ctx, cfg)
}

// runAdmin is `valmind admin reset --username x` (09 §6). It opens the database directly
// and never touches Docker, the lease or the HTTP surface.
func runAdmin(ctx context.Context, args []string, getenv func(string) string) error {
	if len(args) == 0 || args[0] != "reset" {
		return fmt.Errorf("usage: valmind admin reset --username <name>")
	}

	fs := flag.NewFlagSet("admin reset", flag.ContinueOnError)
	username := fs.String("username", "", "username to reset")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *username == "" {
		return fmt.Errorf("--username is required")
	}

	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	slog.SetDefault(cfg.Log.Logger(os.Stderr))

	db, err := store.Open(ctx, cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("database %s: %w", cfg.DB.DSN, err)
	}
	defer func() { _ = db.Close() }()
	if err := store.Migrate(ctx, db.Writer); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	password := auth.RandomPassword()
	params, err := auth.LoadArgon2Params(ctx, db)
	if err != nil {
		return fmt.Errorf("argon2 parameters: %w", err)
	}
	hash, err := auth.HashPassword(password, params)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := db.SetUserPasswordByUsername(ctx, *username, hash); err != nil {
		return fmt.Errorf("reset password for %s: %w", *username, err)
	}
	// Filesystem access already establishes who is allowed to do this; a session left
	// open under the old password is not a reason to trust it further (10 §4.1).
	if u, err := db.UserForLogin(ctx, *username); err == nil && u != nil {
		if err := db.DeleteSessionsForUser(ctx, u.ID); err != nil {
			slog.WarnContext(ctx, "revoking sessions after password reset", slog.Any("error", err))
		}
	}

	if _, err := fmt.Fprintf(
		os.Stdout,
		"Password for %s reset. New password:\n\n    %s\n\n",
		*username,
		password,
	); err != nil {
		return fmt.Errorf("print new password: %w", err)
	}
	return nil
}

// daemon is what the gate produces: the resources every later package is handed.
type daemon struct {
	db      *store.DB
	owner   string
	lease   *store.DaemonLease
	keeper  *crypto.Keeper
	docker  *runtime.Docker
	jobs    *jobs.Engine
	started time.Time
}

// gate is 10 §2, in order. Every failure is fatal and names the check that failed, rather
// than degrading into a panel that half works (01 §6).
//
// One deviation from the table, with its reason: the master key is validated after the
// database rather than before it, because the keeper's HKDF salt lives in kv and the key
// cannot be fully checked without it (10 §3.2). Both remain ahead of anything that
// touches Docker.
func gate(ctx context.Context, cfg *config.Config, getenv func(string) string) (*daemon, error) {
	d := &daemon{started: time.Now()}
	ok := false
	defer func() {
		if !ok {
			d.close(ctx)
		}
	}()

	slog.InfoContext(ctx, "starting valmind",
		slog.String("data_root", cfg.Data.Root), slog.String("listen", cfg.Server.Listen))

	var err error
	if d.db, err = store.Open(ctx, cfg.DB.Driver, cfg.DB.DSN); err != nil {
		return nil, fmt.Errorf("database %s: %w", cfg.DB.DSN, err)
	}
	if err := store.Migrate(ctx, d.db.Writer); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}

	if d.owner, err = store.Owner(ctx, d.db); err != nil {
		return nil, fmt.Errorf("daemon identity: %w", err)
	}
	if d.lease, err = store.AcquireDaemonLease(
		ctx, d.db, cfg.Data.Root, d.owner, cfg.Jobs.LeaseTTL.Std()); err != nil {
		return nil, fmt.Errorf("daemon lease: %w", err)
	}
	// The same owner as the daemon lease (12 §5.2): both are crash markers for this
	// process, and a job's lease_owner must agree with the lease's when WP-15's recovery
	// sweep asks "is the process that claimed this still alive?"
	d.jobs = jobs.New(d.db, d.owner, jobs.Config{
		LeaseTTL:         cfg.Jobs.LeaseTTL.Std(),
		ProgressInterval: cfg.Jobs.ProgressInterval.Std(),
		LogCap:           cfg.Jobs.LogCap,
		RetentionDays:    cfg.Jobs.RetentionDays,
	})
	d.jobs.RegisterCancelPolicy(jobs.KindProvision, api.ProvisionCancelPolicy)

	// 08 §3: probed once at startup rather than per provision, since it never changes for
	// the life of the process. A read failure degrades to the safe, full-copy progress
	// budget rather than blocking startup over an estimate.
	if err := d.db.KVSet(ctx, "data_fs_type", instance.ProbeFSType(cfg.Data.Root)); err != nil {
		return nil, fmt.Errorf("probe data root filesystem: %w", err)
	}

	if d.keeper, err = crypto.Open(ctx, d.db, cfg.Secrets.MasterKeyFile, getenv); err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}

	if d.docker, err = runtime.NewDocker(ctx, cfg.Docker.Endpoint, cfg.Docker.APIVersion); err != nil {
		return nil, fmt.Errorf("container engine: %w", err)
	}
	if err := config.VerifyHostRoot(ctx, d.docker, cfg); err != nil {
		return nil, fmt.Errorf("startup gate: %w", err)
	}
	if err := config.VerifyDataRoot(ctx, cfg); err != nil {
		return nil, fmt.Errorf("startup gate: %w", err)
	}

	ok = true
	return d, nil
}

func (d *daemon) close(ctx context.Context) {
	if d.docker != nil {
		if err := d.docker.Close(); err != nil {
			slog.WarnContext(ctx, "closing docker client", slog.Any("error", err))
		}
	}
	if d.lease != nil {
		if err := d.lease.Release(context.WithoutCancel(ctx)); err != nil {
			slog.WarnContext(ctx, "releasing daemon lease", slog.Any("error", err))
		}
	}
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			slog.WarnContext(ctx, "closing database", slog.Any("error", err))
		}
	}
}

// serve runs until ctx is cancelled or the lease is lost, then shuts down per 11 §10.
func (d *daemon) serve(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	bootstrap := auth.NewBootstrap(d.db)
	pending, err := bootstrap.Pending(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap state: %w", err)
	}
	// Regenerated on every start while unconsumed (10 §6) — not a timer, so a token from
	// an hour-old process is only ever refreshed by restarting it.
	if err := bootstrap.PrintToken(ctx, os.Stdout); err != nil {
		return fmt.Errorf("print setup token: %w", err)
	}

	// 12 §7's retention sweep: one DELETE, once at start, so M1 stays self-limiting ahead
	// of the M4 scheduler's own prune job.
	if err := d.jobs.Sweep(ctx); err != nil {
		return fmt.Errorf("job retention sweep: %w", err)
	}

	health := &api.Health{DB: d.db, Runtime: d.docker}
	router, err := api.NewRouter(cfg, d.db, health, d.keeper, pending, d.jobs, d.docker)
	if err != nil {
		return fmt.Errorf("http surface: %w", err)
	}

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: router,
		// ReadHeaderTimeout only. A server-wide WriteTimeout severs the console
		// WebSocket and truncates backup downloads (C12, 11 §8.1).
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	lost := make(chan error, 1)
	go func() { lost <- d.lease.Renew(ctx) }()

	serveErr := make(chan error, 1)
	go func() {
		slog.InfoContext(ctx, "listening", slog.String("addr", cfg.Server.Listen))
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err)
		}
		return nil
	case err := <-lost:
		if err != nil {
			cancel()
			shutdown(context.WithoutCancel(ctx), srv, health, cfg.Server.ShutdownGrace.Std())
			return err
		}
	case <-ctx.Done():
	}

	slog.InfoContext(ctx, "shutting down", slog.Duration("grace", cfg.Server.ShutdownGrace.Std()))
	shutdown(context.WithoutCancel(ctx), srv, health, cfg.Server.ShutdownGrace.Std())
	return nil
}

// shutdown is 11 §10: drain, stop accepting, wait out the grace period, exit. The daemon
// lease and the database are released by the caller's deferred close.
//
// It takes no Runtime, and that is the point: nothing here can signal a game container.
// Servers keep running and players stay connected while the panel restarts — the panel is
// not load-bearing (C10, G6, 01 §6). Jobs still running at the deadline are abandoned
// rather than failed, and 12 §9 recovers them on the next start.
//
// ctx must be one the shutdown signal has not already cancelled — callers pass
// context.WithoutCancel — or the grace period ends the moment it begins.
func shutdown(ctx context.Context, srv *http.Server, health *api.Health, grace time.Duration) {
	health.Drain()

	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("grace period expired with connections still open", slog.Any("error", err))
	}
}
