package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

const (
	// hostCheckFile holds the token the self-check round-trips through a container.
	hostCheckFile = ".valmin-hostcheck"
	// hostCheckMount is where data.host_root is mounted inside the throwaway container.
	hostCheckMount = "/check"
	// hostCheckTimeout bounds the whole round trip. The image is already present, so this
	// covers a container spawn and nothing else.
	hostCheckTimeout = 60 * time.Second
)

// VerifyHostRoot proves data.host_root and data.root name the same directory by writing a
// fresh token under data.root and reading it back through a container that mounts
// data.host_root (10 §1.2).
//
// It runs on every start, not just the first: a compose file edited six months later
// breaks this silently, and the failure it prevents is a container that starts with an
// empty world directory and generates a brand new world, which looks like success.
//
// The check runs the game image (ADR-048). That image is already required for the panel
// to do anything, so the check adds no dependency and needs no registry access.
func VerifyHostRoot(ctx context.Context, rt runtime.Runtime, cfg *Config) error {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("host_data_root self-check: generate token: %w", err)
	}
	want := hex.EncodeToString(token)

	path := filepath.Join(cfg.Data.Root, hostCheckFile)
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		return fmt.Errorf("host_data_root self-check: write %s: %w", path, err)
	}

	ctx, cancel := context.WithTimeout(ctx, hostCheckTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	code, err := runtime.RunThrowaway(ctx, rt, &runtime.ThrowawaySpec{
		Image:      cfg.Game.Image,
		Entrypoint: []string{"/bin/cat", filepath.Join(hostCheckMount, hostCheckFile)},
		// The panel's own uid, not the fixed 10000 of 08 §2: this check asks whether the
		// mount resolves to the same directory, and reading back a file the panel just
		// wrote should not also depend on a second identity. In production the two are the
		// same uid anyway. Ownership under data.root is VerifyDataRoot's question.
		User: strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Binds: []runtime.Bind{{
			HostPath:      cfg.Data.HostRoot,
			ContainerPath: hostCheckMount,
			ReadOnly:      true,
		}},
		NoNetwork: true,
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	if err != nil {
		return fmt.Errorf("host_data_root self-check: could not run %s: %w", cfg.Game.Image, err)
	}

	if got := strings.TrimSpace(stdout.String()); got != want {
		return hostRootMismatch(cfg, code, got, strings.TrimSpace(stderr.String()))
	}

	slog.InfoContext(ctx, "host_data_root verified",
		slog.String("data_root", cfg.Data.Root),
		slog.String("data_host_root", cfg.Data.HostRoot))
	return nil
}

// hostRootMismatch explains the failure 02 §5 calls the most common bug in this class of
// application. The message names both paths and which one is the host's, because the
// operator's next action depends entirely on that distinction.
func hostRootMismatch(cfg *Config, code int, got, stderr string) error {
	detail := "the file there holds a different token, so data.host_root is some other directory"
	if got == "" {
		detail = fmt.Sprintf("the file was not there (exit %d)", code)
		if stderr != "" {
			detail += ": " + stderr
		}
	}

	return fmt.Errorf(
		"host_data_root self-check failed: %s\n"+
			"  data.root      = %s  (the path this panel writes to)\n"+
			"  data.host_root = %s  (mounted read-only at %s in the check container)\n"+
			"data.host_root must be the path on the host, not the path inside the panel "+
			"container. Left wrong, every instance starts with an empty world directory and "+
			"generates a brand new world, which looks like success (10 §1.2, 02 §5)",
		detail, cfg.Data.Root, cfg.Data.HostRoot, hostCheckMount)
}

// VerifyDataRoot checks that data.root is writable and has room for a game install
// (10 §2). Both failures are otherwise discovered halfway through a 1 GB download.
func VerifyDataRoot(ctx context.Context, cfg *Config) error {
	f, err := os.CreateTemp(cfg.Data.Root, ".valmin-writecheck-*")
	if err != nil {
		return fmt.Errorf("data.root %s is not writable as uid %d: %w",
			cfg.Data.Root, os.Getuid(), err)
	}
	name := f.Name()
	closeErr := f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write check %s: %w", name, err)
	}
	if closeErr != nil {
		return fmt.Errorf("write check %s: %w", name, closeErr)
	}

	free, err := freeSpace(cfg.Data.Root)
	if err != nil {
		return err
	}
	if floor := cfg.Data.FreeSpaceFloorBytes; floor > 0 && free < uint64(floor) {
		return fmt.Errorf(
			"data.root %s has %d bytes free, below the data.free_space_floor_bytes of %d: "+
				"a game install is about 1 GB, and Valheim stops saving below ~6.4 MB "+
				"silently, with the server still running (10 §2, 03 §3.4)",
			cfg.Data.Root, free, floor)
	}

	slog.InfoContext(ctx, "data.root verified",
		slog.String("path", cfg.Data.Root),
		slog.Uint64("free_bytes", free),
		slog.Int("uid", os.Getuid()))
	return nil
}

// freeSpace returns the bytes available to an unprivileged process on path's filesystem.
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("statfs %s reported a block size of %d", path, st.Bsize)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
