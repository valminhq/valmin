//go:build integration

package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// stubImage is the image make test-integration builds. Using it rather than pulling
// busybox keeps the suite hermetic: it needs no registry (06 §4).
const stubImage = "valmin/valheim-stub:dev"

func dockerRuntime(t *testing.T) *Docker {
	t.Helper()
	d, err := NewDocker(t.Context(), "unix:///var/run/docker.sock", "")
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// create makes a container and guarantees its removal, so a failing assertion does not
// leave one behind for the next run to trip over.
func create(t *testing.T, d *Docker, spec *ContainerSpec) string {
	t.Helper()
	id, err := d.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Not t.Context(): it is cancelled before cleanups run, so the container would
	// survive the test that created it.
	t.Cleanup(func() { _ = d.Remove(context.Background(), id, true) })
	return id
}

// This is the host_data_root self-check's exact mechanism: a token written on one side of
// a bind mount and read back through a throwaway container (10 §1.2, C22).
func TestDockerRunThrowawayReadsABindMount(t *testing.T) {
	dir := t.TempDir()
	// The container runs as uid 10000 and the host user is not that, so the check needs
	// the same world-readable path production uses (08 §2).
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}
	const token = "1f0f4c1d-token"
	if err := os.WriteFile(filepath.Join(dir, ".valmin-hostcheck"), []byte(token), 0o644); err != nil {
		t.Fatalf("write token: %v", err)
	}

	var out, errOut bytes.Buffer
	code, err := RunThrowaway(t.Context(), dockerRuntime(t), &ThrowawaySpec{
		Image:      stubImage,
		Entrypoint: []string{"/bin/cat"},
		Cmd:        []string{"/check/.valmin-hostcheck"},
		Binds:      []Bind{{HostPath: dir, ContainerPath: "/check", ReadOnly: true}},
		NoNetwork:  true,
		Stdout:     &out,
		Stderr:     &errOut,
	})
	if err != nil {
		t.Fatalf("run throwaway: %v (stderr %q)", err, errOut.String())
	}
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	if got := out.String(); got != token {
		t.Errorf("read %q from the bind mount, want %q", got, token)
	}
}

// The capability set, no-new-privileges and the swap setting are applied by the adapter
// rather than passed in, so no caller can omit them (ADR-026, 08 §5).
func TestDockerCreateAppliesTheFixedSecurityProperties(t *testing.T) {
	d := dockerRuntime(t)
	const memLimit = 512 << 20

	id := create(t, d, &ContainerSpec{
		Image:       stubImage,
		Entrypoint:  []string{"/bin/true"},
		MemoryBytes: memLimit,
	})

	resp, err := d.cli.ContainerInspect(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	h := resp.HostConfig

	if len(h.CapAdd) != 0 {
		t.Errorf("CapAdd = %v, want empty", h.CapAdd)
	}
	if len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", h.CapDrop)
	}
	if !slices.Contains(h.SecurityOpt, "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", h.SecurityOpt)
	}
	if h.Memory != memLimit || h.MemorySwap != memLimit {
		t.Errorf("Memory = %d, MemorySwap = %d, want both %d (swap disabled)", h.Memory, h.MemorySwap, memLimit)
	}
}

// A1 is set-once: OpenStdin cannot be added to an existing container, so the adapter has
// to carry it through unchanged (07 §3).
func TestDockerCreateCarriesTheSetOnceProperties(t *testing.T) {
	d := dockerRuntime(t)

	id := create(t, d, &ContainerSpec{
		Image:       stubImage,
		Entrypoint:  []string{"/bin/true"},
		User:        "10000:10000",
		OpenStdin:   true,
		StdinOnce:   false,
		TTY:         false,
		StopSignal:  "SIGINT",
		StopTimeout: 120 * time.Second,
	})

	resp, err := d.cli.ContainerInspect(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	cfg := resp.Config

	if !cfg.OpenStdin || cfg.StdinOnce || cfg.Tty {
		t.Errorf("OpenStdin=%v StdinOnce=%v Tty=%v, want true/false/false", cfg.OpenStdin, cfg.StdinOnce, cfg.Tty)
	}
	if cfg.User != "10000:10000" {
		t.Errorf("User = %q, want 10000:10000", cfg.User)
	}
	if cfg.StopSignal != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", cfg.StopSignal)
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != 120 {
		t.Errorf("StopTimeout = %v, want 120", cfg.StopTimeout)
	}
}

// Reconciliation enumerates reality by label, which is the whole reason labels exist
// (A2, 08 §6.1).
func TestDockerListFindsByLabel(t *testing.T) {
	d := dockerRuntime(t)
	instanceID := "wp05-" + t.Name()

	want := create(t, d, &ContainerSpec{
		Image:      stubImage,
		Entrypoint: []string{"/bin/true"},
		Labels:     map[string]string{"io.valmin.managed": "true", "io.valmin.instance.id": instanceID},
	})
	create(t, d, &ContainerSpec{
		Image:      stubImage,
		Entrypoint: []string{"/bin/true"},
		Labels:     map[string]string{"io.valmin.managed": "true"},
	})

	got, err := d.List(t.Context(), map[string]string{
		"io.valmin.managed":     "true",
		"io.valmin.instance.id": instanceID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("got %d containers, want just %s", len(got), want)
	}
	if got[0].Labels["io.valmin.instance.id"] != instanceID {
		t.Errorf("labels = %v, want the instance id", got[0].Labels)
	}
	if got[0].Running {
		t.Error("a container that was never started is not running")
	}
}

func TestDockerWaitReturnsTheExitCode(t *testing.T) {
	d := dockerRuntime(t)

	id := create(t, d, &ContainerSpec{
		Image:      stubImage,
		Entrypoint: []string{"/bin/sh", "-c", "exit 42"},
	})
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	code, err := d.Wait(t.Context(), id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code %d, want 42", code)
	}

	c, err := d.Inspect(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if c.Running || c.ExitCode != 42 {
		t.Errorf("running=%v exit=%d, want false/42", c.Running, c.ExitCode)
	}
	if c.StartedAt.IsZero() || c.FinishedAt.IsZero() {
		t.Errorf("started_at=%v finished_at=%v, want both populated", c.StartedAt, c.FinishedAt)
	}
}

// A container that has never run must report a zero FinishedAt rather than Docker's
// year-1 placeholder, or "has it ever exited" becomes a string comparison upstream.
func TestDockerInspectReportsNeverAsZero(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, &ContainerSpec{Image: stubImage, Entrypoint: []string{"/bin/true"}})

	c, err := d.Inspect(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !c.StartedAt.IsZero() || !c.FinishedAt.IsZero() {
		t.Errorf("started_at=%v finished_at=%v, want both zero", c.StartedAt, c.FinishedAt)
	}
}

func TestDockerMissingContainerIsErrNotFound(t *testing.T) {
	d := dockerRuntime(t)

	_, err := d.Inspect(t.Context(), "valmin-no-such-container")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("inspect a missing container: got %v, want ErrNotFound", err)
	}
}

// With TTY false the log stream is multiplexed, and the two streams must stay
// distinguishable for the console view (E5, 07 §3).
func TestDockerLogsKeepTheStreamsSeparate(t *testing.T) {
	d := dockerRuntime(t)

	id := create(t, d, &ContainerSpec{
		Image:      stubImage,
		Entrypoint: []string{"/bin/sh", "-c", "echo to-stdout; echo to-stderr >&2"},
	})
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := d.Wait(t.Context(), id); err != nil {
		t.Fatalf("wait: %v", err)
	}

	rc, err := d.Logs(t.Context(), id, LogOptions{})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer func() { _ = rc.Close() }()

	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, rc); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "to-stdout" {
		t.Errorf("stdout = %q, want to-stdout", got)
	}
	if got := strings.TrimSpace(errOut.String()); got != "to-stderr" {
		t.Errorf("stderr = %q, want to-stderr", got)
	}
}

func TestDockerStatsReportsALimitedContainer(t *testing.T) {
	d := dockerRuntime(t)
	const memLimit = 64 << 20

	id := create(t, d, &ContainerSpec{
		Image:       stubImage,
		Entrypoint:  []string{"/bin/sh", "-c", "sleep 30"},
		MemoryBytes: memLimit,
	})
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	s, err := d.Stats(t.Context(), id)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if s.MemLimit != memLimit {
		t.Errorf("MemLimit = %d, want the container's own limit %d", s.MemLimit, memLimit)
	}
	if s.MemBytes == 0 || s.MemBytes > memLimit {
		t.Errorf("MemBytes = %d, want between 0 and %d", s.MemBytes, memLimit)
	}
	if s.CPUNanos == 0 {
		t.Error("CPUNanos = 0, want a cumulative counter")
	}

	if err := d.Stop(t.Context(), id, "SIGKILL", time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
