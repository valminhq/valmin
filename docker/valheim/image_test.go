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

// TestImageHomeExistsAndIsWritable is the guard for the boot-time failure of 3 Sep 2026:
// the image recorded /home/valmin in /etc/passwd and never created it, so Unity logged
// `CreateDirectory '/home/valmin' failed` on every start.
//
// `↯` The assertion is against whatever passwd actually says rather than the literal path,
// because the defect was the *gap between the two* — a test naming /home/valmin would still
// pass if someone changed the user's home and left the new one uncreated.
//
// `↯` It runs with --cap-drop ALL, which is what makes the failure reachable: container-root
// could have created the directory on the fly, uid 10000 without CAP_DAC_OVERRIDE cannot
// (08 §5, ADR-026). A probe run with default capabilities would pass against the broken
// image. This is the third container this project has given a HOME it could not write —
// after the SteamCMD throwaway (ADR-112) — so it is asserted rather than remembered.
func TestImageHomeExistsAndIsWritable(t *testing.T) {
	out := docker(t, "run", "--rm",
		"--user", "10000:10000", "--cap-drop", "ALL", "--entrypoint", "sh", image,
		"-c", `home=$(getent passwd 10000 | cut -d: -f6)
			test -n "$home"  || { echo "uid 10000 has no home in passwd"; exit 1; }
			test -d "$home"  || { echo "$home is recorded but does not exist"; exit 1; }
			touch "$home/.valmin-probe" || { echo "$home is not writable by uid 10000"; exit 1; }
			echo "ok $home"`)

	if !strings.HasPrefix(strings.TrimSpace(out), "ok ") {
		t.Errorf("home directory probe failed: %s", strings.TrimSpace(out))
	}
}

// TestImageHasACATrustStore is the guard for the failure of 3 Sep 2026, which cost two days
// and was diagnosed as three different things before this one.
//
// `↯` debian-slim carries no CA bundle, and the game makes HTTPS requests of its own as soon
// as `-crossplay` is set: ZNet.GetPublicIP asks api.ipify.org and friends for the server's
// public address. Without a trust store every handshake fails, the server never learns its
// IP, never registers with PlayFab and never reaches `Game server connected` — logging only
// `Could not extract valid IP address`, hundreds of times. That reads as an IPv6 or network
// fault, which is exactly what it was mistaken for.
//
// The assertion is on a *usable* bundle rather than on the package being installed, because
// the failure is about whether TLS can verify, not about dpkg's opinion.
func TestImageHasACATrustStore(t *testing.T) {
	out := docker(t, "run", "--rm", "--entrypoint", "sh", image, "-c",
		`test -s /etc/ssl/certs/ca-certificates.crt && echo "ok $(wc -l < /etc/ssl/certs/ca-certificates.crt)"`)

	if !strings.HasPrefix(strings.TrimSpace(out), "ok ") {
		t.Errorf("no usable CA bundle in the image: %s\n"+
			"the game's own HTTPS calls (GetPublicIP) fail silently without one", strings.TrimSpace(out))
	}
}
