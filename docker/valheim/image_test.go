//go:build integration

// Verifies the real image's Dockerfile-level contract (08 §4) against a real daemon. It
// cannot exercise the entrypoint itself — there is no game binary in the image, by design
// (08 §4: no game files; provisioning bind-mounts server/ at create time, WP-13) — so this
// asserts what docker inspect can see without ever starting the container.
package valheim_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

// fakeServerDir builds a server/ bind the entrypoint can actually exec into: a stand-in
// game binary that prints the environment it was launched with. The image carries no game
// files by design (08 §4), so this is the only way to exercise the entrypoint end to end.
func fakeServerDir(t *testing.T, withDoorstop bool) string {
	t.Helper()
	dir := t.TempDir()
	// The container runs as uid 10000, which is not the test user; the bind has to be
	// traversable and the binary executable by it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nenv\n"
	if err := os.WriteFile(filepath.Join(dir, "valheim_server.x86_64"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if !withDoorstop {
		return dir
	}
	libs := filepath.Join(dir, "doorstop_libs")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Not a real shared object. ld.so warns that it cannot preload it and carries on, which
	// is enough: what is under test is that the entrypoint exported the variable at all.
	if err := os.WriteFile(filepath.Join(libs, "libdoorstop_x64.so"), []byte("stand-in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runEntrypoint(t *testing.T, serverDir string) string {
	t.Helper()
	return docker(t, "run", "--rm", "-v", serverDir+":/opt/valheim/server", image)
}

// TestVanillaBootGetsNoDoorstopVariables is half of ADR-107: modded-ness is a filesystem
// fact, so a server directory without the Doorstop library must launch exactly as it did
// before this work package existed.
func TestVanillaBootGetsNoDoorstopVariables(t *testing.T) {
	out := runEntrypoint(t, fakeServerDir(t, false))

	for _, name := range []string{"DOORSTOP_ENABLED", "DOORSTOP_TARGET_ASSEMBLY", "LD_PRELOAD"} {
		if strings.Contains(out, name+"=") {
			t.Errorf("a vanilla boot exported %s:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "LD_LIBRARY_PATH=./linux64:") {
		t.Errorf("LD_LIBRARY_PATH did not start with ./linux64:\n%s", out)
	}
	if strings.Contains(out, "doorstop_libs") {
		t.Errorf("a vanilla boot mentioned doorstop_libs:\n%s", out)
	}
}

// TestModdedBootExportsDoorstop is the other half, and it is the one M0's failure makes
// mandatory. The four names and the ordering are the pack's own start_server_bepinex.sh,
// read out of denikson-BepInExPack_Valheim-5.4.2333 — not inferred from the on-disk layout,
// which is precisely how 03 §5.2's silent failure was produced.
//
// `↯` The same image and the same container configuration as the vanilla case: no Env
// change, no recreation. Only the contents of server/ differ.
func TestModdedBootExportsDoorstop(t *testing.T) {
	out := runEntrypoint(t, fakeServerDir(t, true))

	for _, want := range []string{
		"DOORSTOP_ENABLED=1",
		"DOORSTOP_TARGET_ASSEMBLY=./BepInEx/core/BepInEx.Preloader.dll",
		// A bare soname, resolved through LD_LIBRARY_PATH — not a path.
		"LD_PRELOAD=libdoorstop_x64.so:",
		// ./linux64 is prepended last, so it ends up first, exactly as the pack leaves it.
		"LD_LIBRARY_PATH=./linux64:./doorstop_libs:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modded boot did not export %q:\n%s", want, out)
		}
	}
}
