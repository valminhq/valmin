package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/version"
)

// diagnoseConnectTimeout bounds the attempt to reach the container engine. The command
// exists for hosts where that attempt fails, so it must not hang on one.
const diagnoseConnectTimeout = 10 * time.Second

// runDiagnoseCommand writes a support bundle without starting the daemon. Every fact it
// cannot reach becomes an unknown row rather than an error: a panel that does not start
// is the case this command is for. The database is not opened, so nothing the startup
// gate or a job recorded appears.
//
// With no argument it writes to stdout; one argument is the file to write.
func runDiagnoseCommand(ctx context.Context, args []string, getenv func(string) string) error {
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	out := io.Writer(os.Stdout)
	if len(args) > 0 {
		f, err := os.Create(args[0]) //nolint:gosec // the operator names their own output file
		if err != nil {
			return fmt.Errorf("create %s: %w", args[0], err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}

	in := offlineInput(cfg)
	connect, cancel := context.WithTimeout(ctx, diagnoseConnectTimeout)
	docker, dockerErr := runtime.NewDocker(connect, cfg.Docker.Endpoint, cfg.Docker.APIVersion)
	cancel()
	if dockerErr != nil {
		fmt.Fprintf(os.Stderr, "valmind diagnose: container engine unreachable: %v\n", dockerErr)
	} else {
		defer func() { _ = docker.Close() }()
		in.Runtime = docker
	}

	report := diag.Collect(ctx, &in)
	if err := diag.WriteBundle(out, &report, diag.NewConfigView(cfg)); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	return nil
}

// offlineInput gathers what can be read from the configuration and the filesystem alone.
func offlineInput(cfg *config.Config) diag.Input {
	in := diag.Input{
		Config:   cfg,
		Packages: thunderstore.New(cfg.Thunderstore.BaseURL),
		Now:      time.Now().UTC(),
		Build:    version.Current(),
		UID:      os.Getuid(),
		GID:      os.Getgid(),
		FSType:   instance.ProbeFSType(cfg.Data.Root),
	}
	if cfg.Hexium.Enabled {
		in.Hexium = thunderstore.NewBare(cfg.Hexium.BaseURL)
	}
	if free, err := instance.FreeSpace(cfg.Data.Root); err == nil {
		in.FreeBytes = free
	}
	if floor := cfg.Data.FreeSpaceFloorBytes; floor > 0 {
		in.AlarmBytes = uint64(floor)
	}
	return in
}
