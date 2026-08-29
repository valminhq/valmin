// Command valmind is the Valmin panel daemon.
//
// Startup sequence: 10 §2 (validation gate) then 12 §9.1 (lease, job sweep,
// reconciliation, resume intents, streams).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/valminhq/valmin/internal/api"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
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

// daemon is what the gate produces: the resources every later package is handed.
type daemon struct {
	db      *store.DB
	lease   *store.DaemonLease
	keeper  *crypto.Keeper
	docker  *runtime.Docker
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

	owner, err := store.Owner(ctx, d.db)
	if err != nil {
		return nil, fmt.Errorf("daemon identity: %w", err)
	}
	if d.lease, err = store.AcquireDaemonLease(
		ctx, d.db, cfg.Data.Root, owner, cfg.Jobs.LeaseTTL.Std()); err != nil {
		return nil, fmt.Errorf("daemon lease: %w", err)
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

	health := &api.Health{DB: d.db, Runtime: d.docker}
	router, err := api.NewRouter(cfg, health, d.keeper)
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
