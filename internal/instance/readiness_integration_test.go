//go:build integration

// Proves readiness.go against a real daemon and the real demuxed log stream — the fake
// runtime's Logs() is a static snapshot (no true follow), so these are the only tests that
// exercise stdcopy against Docker's actual multiplexed wire format for this package.
package instance_test

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
)

// rawStubSpec bypasses BuildSpec: STUB_MODE is docker/valheim-stub's own test-only env
// var (never a real launch config field), so it has no place in LaunchSpec's allowlist
// (D8) and must be set directly on the ContainerSpec.
func rawStubSpec(t *testing.T, name string, env ...string) *runtime.ContainerSpec {
	t.Helper()
	return &runtime.ContainerSpec{
		Name: name, Image: stubImage, Env: env,
		StopSignal: "SIGINT", StopTimeout: 10 * time.Second,
	}
}

// TestAwaitReadyAgainstARealContainer is 12 §3.3's primary path, against the stub's default
// mode and a real Docker log stream rather than the fake's snapshot.
func TestAwaitReadyAgainstARealContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp14-ready"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	confirmed, err := instance.AwaitReady(t.Context(), d, id, 5*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if !confirmed {
		t.Error("confirmed = false, want true: the stub's default mode announces readiness")
	}
}

// TestAwaitReadyFallsBackAgainstARealContainer is ADR-043's fallback, against the stub's
// no-ready mode.
func TestAwaitReadyFallsBackAgainstARealContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp14-no-ready", "STUB_MODE=no-ready"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	confirmed, err := instance.AwaitReady(t.Context(), d, id, 2*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if confirmed {
		t.Error("confirmed = true, but the stub never announced readiness")
	}
}

// TestAwaitReadyErrorsAgainstARealExitingContainer is 12 §3.3's failure path: the container
// exiting inside the window is a real start failure.
func TestAwaitReadyErrorsAgainstARealExitingContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp14-exit-early", "STUB_MODE=exit-early"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, err := instance.AwaitReady(t.Context(), d, id, 10*time.Second, 10*time.Second); err == nil {
		t.Fatal("AwaitReady: want an error, the stub exits at t+2s")
	}
}

// TestSawSaveLineAgainstARealContainer is 12 §3.4's positive path.
func TestSawSaveLineAgainstARealContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp14-clean-stop"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLog(t, id, "Game server connected")

	if err := d.Stop(t.Context(), id, "SIGINT", 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	clean, err := instance.SawSaveLine(t.Context(), d, id)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if !clean {
		t.Error("clean = false, want true: the stub's default mode reaches the full save literal")
	}
}

// TestSawSaveLineFalseAgainstARealNoSaveFinishContainer is B2's real-daemon proof: the stub
// stops right after "finishing" and must not satisfy the "finished" pattern.
func TestSawSaveLineFalseAgainstARealNoSaveFinishContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp14-no-save-finish", "STUB_MODE=no-save-finish"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLog(t, id, "Game server connected")

	if err := d.Stop(t.Context(), id, "SIGINT", 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	clean, err := instance.SawSaveLine(t.Context(), d, id)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if clean {
		t.Error("clean = true, want false: the stub never reached the full literal")
	}
}
