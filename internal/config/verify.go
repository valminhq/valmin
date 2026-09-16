package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

	// gameNetworkCheckPurpose labels the probe container, so one left behind by a killed
	// panel is identifiable and swept.
	gameNetworkCheckPurpose = "game-network-check"
	// gameNetworkCheckTimeout bounds the probe and is how long its container sleeps.
	gameNetworkCheckTimeout = 30 * time.Second
	// gameNetworkDialTimeout separates a refusal from a dropped packet: a route that exists
	// answers at once, so anything slower is the failure being tested for.
	gameNetworkDialTimeout = 3 * time.Second
)

// VerifyHostRoot proves data.host_root and data.root name the same directory by writing a
// fresh token under data.root and reading it back through a container that mounts data.host_root
// (10 §1.2).
//
// It runs on every start, not just the first: a compose file edited later can break this
// silently, and the failure it prevents is a container starting with an empty world directory
// and generating a new one, which looks like success.
//
// The check runs the game image, already required for the panel to do anything (ADR-048).
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
		Purpose:    "host-data-root-check",
		Image:      cfg.Game.Image,
		Entrypoint: []string{"/bin/cat", filepath.Join(hostCheckMount, hostCheckFile)},
		// The panel's own uid, not the fixed 10000 of 08 §2: this checks whether the mount
		// resolves to the same directory, not ownership, which is VerifyDataRoot's question.
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

// VerifyGameNetwork proves the daemon can reach a container on game.network, which is where it
// dials the command channel (07 §2.2). It creates a throwaway there, dials it on probePort, and
// reads the answer: a refusal proves the route, since nothing is listening; a timeout is a
// dropped packet, which is what a daemon on some other network gets (ADR-190).
//
// An empty game.network leaves containers on Docker's default bridge and skips the check.
func VerifyGameNetwork(ctx context.Context, rt runtime.Runtime, cfg *Config, probePort int) error {
	if cfg.Game.Network == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, gameNetworkCheckTimeout)
	defer cancel()

	// The container has to outlive the dial, so it is created, dialled and removed in one
	// scope: an address whose container is already gone answers nothing, which reads as the
	// dropped packet this check exists to report.
	id, err := rt.Create(ctx, &runtime.ContainerSpec{
		Image:      cfg.Game.Image,
		Entrypoint: []string{"/bin/sleep", strconv.Itoa(int(gameNetworkCheckTimeout.Seconds()))},
		User:       strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		Labels:     map[string]string{runtime.LabelThrowaway: gameNetworkCheckPurpose},
		Network:    cfg.Game.Network,
	})
	if err != nil {
		return gameNetworkProbeFailed(cfg, err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gameNetworkDialTimeout)
		defer cancel()
		if err := rt.Remove(removeCtx, id, true); err != nil {
			slog.WarnContext(ctx, "throwaway container not removed",
				slog.String("container_id", id), slog.String("purpose", gameNetworkCheckPurpose),
				slog.Any("error", err))
		}
	}()

	if err := rt.Start(ctx, id); err != nil {
		return gameNetworkProbeFailed(cfg, err)
	}
	probe, err := rt.Inspect(ctx, id)
	if err != nil {
		return gameNetworkProbeFailed(cfg, err)
	}
	if len(probe.NetworkAddresses) == 0 {
		return gameNetworkProbeFailed(cfg,
			errors.New("the probe container reports no address on it"))
	}
	address := net.JoinHostPort(probe.NetworkAddresses[0], strconv.Itoa(probePort))

	dialer := net.Dialer{Timeout: gameNetworkDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err == nil {
		_ = conn.Close()
		return gameNetworkUnexpectedListener(cfg, address)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		slog.InfoContext(ctx, "game network verified",
			slog.String("network", cfg.Game.Network), slog.String("probe_address", address))
		return nil
	}
	return gameNetworkUnreachable(cfg, address, err)
}

// gameNetworkProbeFailed reports a probe that never got as far as a dial. Every one of those
// is the same operator question — does this network exist and can the panel use it — so they
// carry one message, whichever Docker call reported it.
func gameNetworkProbeFailed(cfg *Config, err error) error {
	return fmt.Errorf(
		"game network self-check: could not run a probe container on %q: %w\n"+
			"the network is created by the deployment, never by the panel: "+
			"`docker network create %s`, with the panel attached to it (ADR-190)",
		cfg.Game.Network, err, cfg.Game.Network)
}

func gameNetworkUnreachable(cfg *Config, address string, err error) error {
	return fmt.Errorf(
		"game network self-check failed: %s did not answer: %w\n"+
			"  game.network = %s\n"+
			"a refusal would prove the route; this is a dropped packet, so the daemon is not "+
			"on that network. Attach it — on the shipped deployment the panel joins "+
			"%s alongside its own networks — or every command to a running server times out "+
			"(07 §2.2, ADR-190)",
		address, err, cfg.Game.Network, cfg.Game.Network)
}

func gameNetworkUnexpectedListener(cfg *Config, address string) error {
	return fmt.Errorf(
		"game network self-check failed: %s accepted a connection\n"+
			"  game.network = %s\n"+
			"the probe container listens on nothing, so another container answered: the "+
			"address is being reused and the command channel would reach the wrong server "+
			"(07 §2.2)",
		address, cfg.Game.Network)
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
