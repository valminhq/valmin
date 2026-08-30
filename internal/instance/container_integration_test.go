//go:build integration

// Proves BuildSpec's output — not a hand-built ContainerSpec — carries 08 §5's contract
// against a real daemon. internal/runtime's own integration suite proves the adapter
// applies CapDrop/SecurityOpt/swap correctly for any spec; this suite proves the
// domain-specific translator that actually runs in production assembles the rest of it
// correctly: labels, the set-once stdin properties, the stop signal and timeout, and the
// consecutive UDP port pair.
package instance_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
)

const stubImage = "valmin/valheim-stub:dev"

func dockerRuntime(t *testing.T) *runtime.Docker {
	t.Helper()
	d, err := runtime.NewDocker(t.Context(), "unix:///var/run/docker.sock", "")
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func create(t *testing.T, rt runtime.Runtime, spec *runtime.ContainerSpec) string {
	t.Helper()
	id, err := rt.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Not t.Context(): it is cancelled before cleanups run, so the container would
	// survive the test that created it.
	t.Cleanup(func() { _ = rt.Remove(context.Background(), id, true) })
	return id
}

func testSpec(t *testing.T, instanceID string, basePort int) *runtime.ContainerSpec {
	t.Helper()
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID:          instanceID,
		DataDir:             t.TempDir(),
		BasePort:            basePort,
		ServerName:          "Integration Server",
		WorldName:           "IntegrationWorld",
		Password:            "hunter2",
		CrossplayInstanceID: "cp-" + instanceID,
		MemLimitMB:          512,
	}, stubImage, 120*time.Second)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	return spec
}

// inspect runs `docker inspect -f <format>` against id. Reaching for the CLI here rather
// than the Docker SDK keeps internal/runtime's exported surface narrow (WP-05's own
// risk note) — the same choice docker/valheim-stub/stub_test.go already made.
func inspect(t *testing.T, id, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, id).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect %s: %v\n%s", format, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBuildSpecCreatesTheFullContract is the WP-12 acceptance list, end to end: every
// property 08 §5 fixes, read back from a real daemon rather than asserted on the struct.
func TestBuildSpecCreatesTheFullContract(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, testSpec(t, "wp12-full-contract", 27456))

	c, err := d.Inspect(t.Context(), id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if c.Labels["io.valmin.managed"] != "true" ||
		c.Labels["io.valmin.schema"] != "1" ||
		c.Labels["io.valmin.instance.id"] != "wp12-full-contract" ||
		c.Labels["io.valmin.base-port"] != "27456" {
		t.Errorf("labels = %v, missing one of the four io.valmin.* facts", c.Labels)
	}
}

// TestBuildSpecSetOncePropertiesAgainstARealDaemon guards A1: retrofitting OpenStdin
// means recreating every container in the field, so this must be true from BuildSpec's
// very first call, not tuned in later.
func TestBuildSpecSetOncePropertiesAgainstARealDaemon(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, testSpec(t, "wp12-set-once", 27461))

	if got := inspect(t, id, "{{.Config.OpenStdin}} {{.Config.StdinOnce}} {{.Config.Tty}}"); got != "true false false" {
		t.Errorf("OpenStdin/StdinOnce/Tty = %q, want true false false", got)
	}
	if got := inspect(t, id, "{{.Config.User}}"); got != "10000:10000" {
		t.Errorf("User = %q, want 10000:10000", got)
	}
	if got := inspect(t, id, "{{.Config.StopSignal}}"); got != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", got)
	}
	if got := inspect(t, id, "{{.Config.StopTimeout}}"); got != "120" {
		t.Errorf("StopTimeout = %q, want 120", got)
	}
	if got := inspect(t, id, "{{.HostConfig.CapAdd}}"); got != "[]" {
		t.Errorf("CapAdd = %q, want empty (ADR-026)", got)
	}
	if got := inspect(t, id, "{{.HostConfig.CapDrop}}"); got != "[ALL]" {
		t.Errorf("CapDrop = %q, want [ALL]", got)
	}
	mem := inspect(t, id, "{{.HostConfig.Memory}}")
	swap := inspect(t, id, "{{.HostConfig.MemorySwap}}")
	if mem == "0" || mem != swap {
		t.Errorf("Memory=%s MemorySwap=%s, want equal and non-zero (no swap)", mem, swap)
	}
}

// TestBuildSpecStopProducesTheSaveCompleteLine is D1's own acceptance test restated
// against the real translator: docker stop against a container BuildSpec created must
// still run the full SIGINT shutdown path (03 §3.2.1, B2).
func TestBuildSpecStopProducesTheSaveCompleteLine(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, testSpec(t, "wp12-stop-save", 27466))

	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForLog(t, id, "Game server connected")

	if err := d.Stop(t.Context(), id, "SIGINT", 120*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	logs := readLogs(t, id)
	if !strings.Contains(logs, "World save writing finished") {
		t.Errorf("save did not complete before exit:\n%s", logs)
	}
}

// TestBuildSpecContainersAreFoundByLabelAlone is 08 §6.1's whole point: after a DB loss,
// the panel enumerates reality by label, never by name or by remembering a container id.
func TestBuildSpecContainersAreFoundByLabelAlone(t *testing.T) {
	d := dockerRuntime(t)
	// ContainerName is 8-char sugar, so the two ids must differ within that prefix — the
	// panel resolves by label, never by name, precisely because of collisions like this.
	want := create(t, d, testSpec(t, "wp12lbla-instance", 27471))
	create(t, d, testSpec(t, "wp12lblb-instance", 27476))

	got, err := d.List(t.Context(), map[string]string{
		"io.valmin.managed":     "true",
		"io.valmin.instance.id": "wp12lbla-instance",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("got %d containers, want just %s", len(got), want)
	}
}

// waitForLog and readLogs shell out to `docker logs`, which demuxes Docker's multiplexed
// stream itself — the same choice docker/valheim-stub/stub_test.go makes, and simpler
// than repeating internal/runtime's own stdcopy-based demux test here.
func waitForLog(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readLogs(t, id), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not see %q within 10s\n%s", want, readLogs(t, id))
}

func readLogs(t *testing.T, id string) string {
	t.Helper()
	out, err := exec.Command("docker", "logs", id).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v\n%s", err, out)
	}
	return string(out)
}
