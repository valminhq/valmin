//go:build integration

// Verifies the real image's Dockerfile-level contract (08 §4) against a real daemon. It
// cannot exercise the entrypoint itself — there is no game binary in the image, by design
// (08 §4: no game files; provisioning bind-mounts server/ at create time, WP-13) — so this
// asserts what docker inspect can see without ever starting the container.
package valheim_test

import (
	"os/exec"
	"strings"
	"testing"
)

const image = "valmin/valheim:dev"

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestImageStopSignalIsSIGINT guards A1/B1 at the image layer, belt-and-braces with the
// StopSignal instance.BuildSpec sets explicitly on every container (08 §5).
func TestImageStopSignalIsSIGINT(t *testing.T) {
	if got := strings.TrimSpace(docker(t, "inspect", "-f", "{{.Config.StopSignal}}", image)); got != "SIGINT" {
		t.Errorf("StopSignal = %q, want SIGINT", got)
	}
}

// TestImageRunsAsUID10000 guards A3: the image's own default user, independent of the
// User BuildSpec also sets on every container it creates.
func TestImageRunsAsUID10000(t *testing.T) {
	id := strings.TrimSpace(docker(t, "create", image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	if got := strings.TrimSpace(docker(t, "inspect", "-f", "{{.Config.User}}", id)); got != "valmin" {
		t.Errorf("User = %q, want valmin (uid 10000)", got)
	}
	got := strings.TrimSpace(docker(t, "run", "--rm", "--entrypoint", "id", image, "-u"))
	if got != "10000" {
		t.Errorf("uid = %q, want 10000", got)
	}
}

// TestImageCarriesCat guards 08 §4.2: the host_data_root self-check runs this image
// because it must never depend on a registry, which only holds if /bin/cat is already in it.
func TestImageCarriesCat(t *testing.T) {
	out := strings.TrimSpace(docker(t, "run", "--rm", "--entrypoint", "/bin/cat", image, "--version"))
	if !strings.Contains(out, "cat (GNU coreutils)") {
		t.Errorf("expected GNU cat, got %q", out)
	}
}
