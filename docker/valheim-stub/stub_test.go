//go:build integration

// Verifies the stub image's runtime contract against a real Docker daemon. Run with
// `make test-integration`, which builds the image first.
package stub_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const image = "valmin/valheim-stub:dev"

// saveComplete is the full literal the backup quiesce blocks on (03 §3.2.1, B2).
const saveComplete = "World save writing finished"

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// start runs the stub detached and returns its container id and a cleanup func.
func start(t *testing.T, env ...string) string {
	t.Helper()
	args := make([]string, 0, 4+2*len(env))
	args = append(args, "run", "-d", "--rm=false")
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	id := strings.TrimSpace(docker(t, args...))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

// logTimeout bounds every waitForLog. The stub prints its whole startup sequence in
// well under a second; this is slack for a loaded CI machine, not a tuning knob.
const logTimeout = 10 * time.Second

// waitForLog polls the container log until it contains want.
func waitForLog(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(logTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(docker(t, "logs", id), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not see %q within %s\n%s", want, logTimeout, docker(t, "logs", id))
}

func exitCode(t *testing.T, id string) string {
	t.Helper()
	return strings.TrimSpace(docker(t, "inspect", "-f", "{{.State.ExitCode}}", id))
}

// TestStopSignalRunsSaveToCompletion is WP-M1-01's acceptance criterion: docker stop
// delivers SIGINT, the save sequence completes, and the container exits cleanly.
func TestStopSignalRunsSaveToCompletion(t *testing.T) {
	id := start(t)
	waitForLog(t, id, "Game server connected")

	started := time.Now()
	docker(t, "stop", "-t", "120", id)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("stop took %s, want under 10s", elapsed)
	}

	logs := docker(t, "logs", id)
	if !strings.Contains(logs, saveComplete) {
		t.Errorf("save did not complete\n%s", logs)
	}
	if got := exitCode(t, id); got != "0" {
		t.Errorf("exit code = %s, want 0", got)
	}
}

// TestStopSignalIsSIGINT guards INVARIANTS A1/B1: the image declares SIGINT, not the
// Docker default of SIGTERM.
func TestStopSignalIsSIGINT(t *testing.T) {
	id := start(t)
	if got := strings.TrimSpace(docker(t, "inspect", "-f", "{{.Config.StopSignal}}", id)); got != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", got)
	}
}

// TestSavePhasesAreDistinguishable guards INVARIANTS B2. Four save phases share the
// prefix "World save writing" and two share the stem "finish", so a matcher that is not
// the full literal fires on "finishing" and archives a half-written world.
func TestSavePhasesAreDistinguishable(t *testing.T) {
	id := start(t, "STUB_MODE=no-save-finish")
	waitForLog(t, id, "Game server connected")
	docker(t, "stop", "-t", "120", id)

	logs := docker(t, "logs", id)
	if !strings.Contains(logs, "World save writing finishing") {
		t.Fatalf("fixture did not reach the finishing phase\n%s", logs)
	}
	if strings.Contains(logs, saveComplete) {
		t.Errorf("no-save-finish emitted %q; the B2 fixture is broken", saveComplete)
	}
}

// TestTwoLogGrammars guards INVARIANTS E4: networking lines carry an
// MM/DD/YYYY HH:MM:SS: prefix and Unity Debug.Log lines do not, so a start-anchored
// pattern silently misses every readiness line.
func TestTwoLogGrammars(t *testing.T) {
	id := start(t)
	waitForLog(t, id, "Game server connected")
	docker(t, "stop", "-t", "120", id)

	var prefixed, bare bool
	for _, line := range strings.Split(docker(t, "logs", id), "\n") {
		switch {
		case strings.Contains(line, "Game server connected"):
			prefixed = !strings.HasPrefix(strings.TrimSpace(line), "Game server connected")
		case strings.Contains(line, saveComplete):
			bare = strings.HasPrefix(strings.TrimSpace(line), saveComplete)
		}
	}
	if !prefixed {
		t.Error("readiness line was not timestamp-prefixed")
	}
	if !bare {
		t.Error("save-complete line was unexpectedly prefixed")
	}
}

// TestExitEarlyIsAFailedStart backs the 12 §3.3 rule that only a container exiting
// inside the readiness window is a real start failure.
func TestExitEarlyIsAFailedStart(t *testing.T) {
	id := start(t, "STUB_MODE=exit-early")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(docker(t, "inspect", "-f", "{{.State.Running}}", id)) == "false" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := exitCode(t, id); got != "1" {
		t.Errorf("exit code = %s, want 1", got)
	}
}

// TestSingularPluginLine guards INVARIANTS E9: one plugin logs "plugin", singular, so
// `(\d+) plugins to load` reports zero mods loaded on a single-mod instance.
func TestSingularPluginLine(t *testing.T) {
	id := start(t, "STUB_MODE=modded", "STUB_PLUGINS=1")
	waitForLog(t, id, "plugin to load")

	logs := docker(t, "logs", id)
	if strings.Contains(logs, "1 plugins to load") {
		t.Error("stub emitted the plural form for a single plugin")
	}
}

// TestStubDetectsModdedFromTheFilesystem mirrors the real image's contract (ADR-107,
// ADR-063): modded-ness is the presence of the Doorstop library under server/, not a flag
// the caller set. Asserting it here is what lets the panel's own integration tests trust
// that a modded instance produces chainloader lines for the reason the real server would.
func TestStubDetectsModdedFromTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	libs := filepath.Join(dir, "doorstop_libs")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "libdoorstop_x64.so"), []byte("stand-in\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := strings.TrimSpace(docker(t, "run", "-d", "--rm=false",
		"-v", dir+":/opt/valheim/server", image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	// No STUB_MODE was set: the chainloader sequence has to come from the bind alone.
	waitForLog(t, id, "Chainloader startup complete")
	waitForLog(t, id, "plugin to load")
}

// TestStubStaysVanillaWithoutDoorstop is the negative half, and it is what makes the E1
// assertion testable at all: a modded instance record over a server directory with no
// BepInEx is exactly the silent failure the panel has to notice.
func TestStubStaysVanillaWithoutDoorstop(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(docker(t, "run", "-d", "--rm=false",
		"-v", dir+":/opt/valheim/server", image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	waitForLog(t, id, "Game server connected")
	if out := docker(t, "logs", id); strings.Contains(out, "BepInEx") {
		t.Errorf("a vanilla bind produced BepInEx lines:\n%s", out)
	}
}
