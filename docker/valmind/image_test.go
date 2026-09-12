//go:build integration

// Verifies the panel image's contract (08 §2, 10 §2) against a real daemon: the identity it
// runs as, the tools the code actually calls, and the absence of everything the build stages
// were there to leave behind.
package valmind_test

import (
	"os/exec"
	"strings"
	"testing"
)

const image = "valmin/valmind:dev"

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// run executes one command through the image's entrypoint, which is how the daemon itself
// starts and therefore the only honest way to observe what it inherits.
func run(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(docker(t, append([]string{"run", "--rm", image}, args...)...))
}

// TestImageRunsAsUID10000WithUmask002 guards A3 and 08 §2.1. The uid is what makes the panel
// own every file it writes into a bind mount; the umask is what leaves those files readable
// to the operator's own account through gid 10000.
func TestImageRunsAsUID10000WithUmask002(t *testing.T) {
	id := strings.TrimSpace(docker(t, "create", image))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	if got := strings.TrimSpace(docker(t, "inspect", "-f", "{{.Config.User}}", id)); got != "valmin" {
		t.Errorf("User = %q, want valmin (uid 10000)", got)
	}
	if got := run(t, "id", "-u"); got != "10000" {
		t.Errorf("uid = %q, want 10000", got)
	}
	if got := run(t, "sh", "-c", "umask"); got != "0002" {
		t.Errorf("umask = %q, want 0002", got)
	}
}

// TestImageCarriesTheToolsTheCodeCalls guards the runtime dependencies that are invisible
// until they are missing: GNU cp, because provisioning clones the game with
// `cp -a --reflink=auto` (08 §3), and a CA bundle, because every host the panel reaches is
// HTTPS — Thunderstore, Steam's build endpoint, and webhook destinations (ADR-167).
func TestImageCarriesTheToolsTheCodeCalls(t *testing.T) {
	if out := run(t, "sh", "-c", "cp --help"); !strings.Contains(out, "--reflink") {
		t.Error("cp does not support --reflink; provisioning's clone would fail on every instance")
	}
	if out := run(t, "sh", "-c", "ls /etc/ssl/certs/ca-certificates.crt"); !strings.Contains(out, ".crt") {
		t.Errorf("no CA bundle: %q", out)
	}
}

// TestImageCarriesNoBuildEnvironment asserts the artefact is a runtime, not a checkout: the
// SPA is built and embedded, so nothing that built it belongs in the image.
func TestImageCarriesNoBuildEnvironment(t *testing.T) {
	for _, tool := range []string{"node", "npm", "go"} {
		if out := run(t, "sh", "-c", "command -v "+tool+" || true"); out != "" {
			t.Errorf("%s is in the runtime image, at %s", tool, out)
		}
	}
	for _, dir := range []string{"/src", "/out", "/root/go"} {
		if out := run(t, "sh", "-c", "ls -d "+dir+" 2>/dev/null || true"); out != "" {
			t.Errorf("build directory %s survived into the runtime image", dir)
		}
	}
}

// TestImageReportsItsOwnBuild guards what M6 cannot publish a candidate without: the version
// subcommand answers before any configuration is read, and the SPA is embedded in the binary
// that answers it rather than served from a directory beside it.
func TestImageReportsItsOwnBuild(t *testing.T) {
	if out := run(t, "/usr/local/bin/valmind", "version"); out == "" {
		t.Error("valmind version printed nothing")
	}
	out := run(t, "sh", "-c", "grep -c build/app/index.html /usr/local/bin/valmind || true")
	if out == "0" || out == "" {
		t.Error("the binary does not carry the embedded SPA; the image was built without it")
	}
}

// TestImageDeclaresAHealthCheck guards 11 §10's probe reaching Docker: the daemon answers it
// itself, so the image needs no HTTP client of its own.
func TestImageDeclaresAHealthCheck(t *testing.T) {
	got := strings.TrimSpace(docker(t, "inspect", "-f", "{{.Config.Healthcheck.Test}}", image))
	if !strings.Contains(got, "valmind") || !strings.Contains(got, "healthcheck") {
		t.Errorf("Healthcheck.Test = %q, want the daemon's own healthcheck subcommand", got)
	}
}
