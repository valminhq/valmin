package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// bepinexZip is the framework package's real shape: everything under one wrapper directory
// containing BepInEx/ (03 §6.4 heuristic 1), including the config file whose console key
// 03 §5.5 asserts on.
func bepinexZip() map[string]string {
	return map[string]string{
		"manifest.json": `{"name":"BepInExPack_Valheim"}`,
		"BepInExPack_Valheim/BepInEx/core/BepInEx.Preloader.dll": "preloader",
		// The real package ships this key already reading true, which is why 03 §5.5's
		// edit is the rare path rather than the normal one — an operator's own file is
		// where it earns its keep.
		"BepInExPack_Valheim/BepInEx/config/BepInEx.cfg": "[Logging.Console]\n" +
			"## Enables showing a console for log output.\nEnabled = true\n",
		"BepInExPack_Valheim/doorstop_libs/libdoorstop_x64.so": "doorstop",
		"BepInExPack_Valheim/winhttp.dll":                      "winhttp",
	}
}

// aPlainMod is a mod that names no dependencies at all — the case the auto-install rule
// exists for: a vanilla instance getting its first mod has to gain BepInEx even though
// nothing in the closure asked for it.
func aPlainMod() modPackageFixture {
	return modPackageFixture{
		fullName: "Smoothbrain-Sailing", version: "1.1.8",
		files: map[string]string{"manifest.json": "{}", "Sailing.dll": "sailing"},
	}
}

func installOK(t *testing.T, rt *Router, admin *store.User, fullName, version string) {
	t.Helper()
	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, fullName, version), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "succeeded" {
		t.Fatalf("install of %s-%s = %+v, want succeeded", fullName, version, got)
	}
}

func instanceRow(t *testing.T, db *store.DB) *store.Instance {
	t.Helper()
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil || inst == nil {
		t.Fatalf("read inst-a: %v", err)
	}
	return inst
}

// TestFirstModOnAVanillaInstanceInstallsBepInEx covers the auto-install rule. Sailing names
// no dependencies, so nothing in its closure asks for the framework — the panel adds it, and
// marks it a dependency because the operator did not ask for it either.
func TestFirstModOnAVanillaInstanceInstallsBepInEx(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t,
		aPlainMod(),
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
	)

	installOK(t, rt, admin, "Smoothbrain-Sailing", "1.1.8")

	rows := installedRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("instance_mods = %+v, want the mod and the framework", rows)
	}
	if got := rows[BepInExPack].InstalledAs; got != store.InstalledDependency {
		t.Errorf("%s installed_as = %q, want dependency", BepInExPack, got)
	}

	// The framework merges at the server root, and doorstop_libs is what the entrypoint
	// autodetects (ADR-107) — without it the container would keep booting vanilla.
	for _, want := range []string{"doorstop_libs/libdoorstop_x64.so", "BepInEx/core/BepInEx.Preloader.dll"} {
		if _, err := os.Stat(filepath.Join(dataDir, "server", filepath.FromSlash(want))); err != nil {
			t.Errorf("%s was not placed: %v", want, err)
		}
	}

	inst := instanceRow(t, db)
	if !inst.Modded {
		t.Error("modded is not set after installing BepInEx")
	}
	if inst.BepInExVersion == nil || *inst.BepInExVersion != "5.4.2333" {
		t.Errorf("bepinex_version = %v, want 5.4.2333", inst.BepInExVersion)
	}
}

// TestASecondModDoesNotReinstallBepInEx: the instance is already modded, so the auto-install
// rule does not fire again and the framework's own row is untouched.
func TestASecondModDoesNotReinstallBepInEx(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t,
		aPlainMod(),
		modPackageFixture{
			fullName: "Azumatt-AzuCraftyBoxes", version: "1.8.15",
			files: map[string]string{"manifest.json": "{}", "AzuCraftyBoxes.dll": "azu"},
		},
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
	)

	installOK(t, rt, admin, "Smoothbrain-Sailing", "1.1.8")
	installOK(t, rt, admin, "Azumatt-AzuCraftyBoxes", "1.8.15")

	rows := installedRows(t, db)
	if len(rows) != 3 {
		t.Fatalf("instance_mods = %+v, want two mods and one framework", rows)
	}
	if got := rows[BepInExPack].Version; got != "5.4.2333" {
		t.Errorf("%s version = %q, want it untouched", BepInExPack, got)
	}
}

// TestAutoInstallDoesNotBumpAVersionTheClosureAlreadyNames. Adding the framework
// unconditionally would make its *latest* version a request, and 03 §6.3 resolves a diamond
// upward — so a mod pinning 5.4.2333 would silently get whatever the index calls latest.
// The rule only fires when the closure does not already name the package.
func TestAutoInstallDoesNotBumpAVersionTheClosureAlreadyNames(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t,
		modPackageFixture{
			fullName: "ValheimModding-Jotunn", version: "2.29.2",
			deps:  []string{BepInExPack + "-5.4.2333"},
			files: map[string]string{"manifest.json": "{}", "plugins/Jotunn.dll": "jotunn"},
		},
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
		modPackageFixture{fullName: BepInExPack, version: "5.4.2400", files: bepinexZip()},
	)

	installOK(t, rt, admin, "ValheimModding-Jotunn", "2.29.2")

	if got := installedRows(t, db)[BepInExPack].Version; got != "5.4.2333" {
		t.Errorf("%s version = %q, want the 5.4.2333 the closure named, not the index's latest", BepInExPack, got)
	}
}

// TestInstallTurnsOnConsoleLoggingItDidNotOverwrite is 03 §5.5 against the case that
// actually needs it. An install never overwrites an existing config (03 §6.4), so an
// operator's own BepInEx.cfg with the console off survives the copy — and then the panel
// would never see a chainloader line. The surgical edit runs afterwards, on that file.
func TestInstallTurnsOnConsoleLoggingItDidNotOverwrite(t *testing.T) {
	rt, _, admin, _, dataDir := installWorld(t,
		aPlainMod(),
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
	)

	const operators = "## the operator's own file\n[Logging.Console]\nEnabled = false\nPreventClose = true\n"
	writeServerFile(t, dataDir, "BepInEx/config/BepInEx.cfg", operators)

	installOK(t, rt, admin, "Smoothbrain-Sailing", "1.1.8")

	got, err := os.ReadFile(filepath.Join(dataDir, "server", "BepInEx", "config", "BepInEx.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(operators, "Enabled = false", "Enabled = true", 1)
	if string(got) != want {
		t.Errorf("BepInEx.cfg =\n%q\nwant\n%q", got, want)
	}
}

// startInstance runs a start job and returns its terminal view.
func startInstance(t *testing.T, rt *Router, admin *store.User) jobView {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	return waitJob(t, rt, admin, accepted.JobID)
}

func jobLog(t *testing.T, db *store.DB, jobID string) string {
	t.Helper()
	j, err := db.JobByID(t.Context(), jobID)
	if err != nil || j == nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}
	if j.Log == nil {
		return ""
	}
	return *j.Log
}

// shrinkPluginWindow keeps the E1 tests from waiting out the real five-second window.
func shrinkPluginWindow(t *testing.T) {
	t.Helper()
	previous := pluginLoadWindow
	pluginLoadWindow = 50 * time.Millisecond
	t.Cleanup(func() { pluginLoadWindow = previous })
}

// TestAModdedInstanceWithNoPluginLineWarnsAndStaysRunning is E1, and it is the whole reason
// this assertion exists. The measured failure was a server that booted perfectly, logged no
// error, and loaded zero mods; nothing about that is visible from the container's state. The
// panel looks for the line on purpose, and says so when it is absent.
//
// A warning, not a failure (ADR-043's precedent): the instance reaches `running` and
// stays there.
func TestAModdedInstanceWithNoPluginLineWarnsAndStaysRunning(t *testing.T) {
	shrinkPluginWindow(t)
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `UPDATE instances SET modded = TRUE, bepinex_version = '5.4.2333' WHERE id = 'inst-a'`)
	// Ready, but not one BepInEx line — a server running vanilla under a modded record.
	fake.Get(containerID).Stdout("Game server connected\n")

	got := startInstance(t, rt, admin)
	if got.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded; E1 is a warning, not a failure", got)
	}
	if instanceRow(t, db).State != "running" {
		t.Errorf("state = %q, want running", instanceRow(t, db).State)
	}
	if log := jobLog(t, db, got.JobID); !strings.Contains(log, "BepInEx never reported a plugin count") {
		t.Errorf("job log does not warn about the missing plugin line:\n%s", log)
	}
	// The warning is asserted on the job's log, not its message: 12 §7 writes the log
	// column exactly once at Finish, while the message column is the throttled progress
	// field and carries whatever last got past the interval. The live job.{id} topic sees
	// the warning in the message too; the log is what survives.
}

// TestAModdedInstanceThatLoadsPluginsDoesNotWarn is the positive control: the same modded
// instance, with the chainloader line the stub and the real server both print.
func TestAModdedInstanceThatLoadsPluginsDoesNotWarn(t *testing.T) {
	shrinkPluginWindow(t)
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `UPDATE instances SET modded = TRUE, bepinex_version = '5.4.2333' WHERE id = 'inst-a'`)
	// Singular at one plugin — E9's `plugins?`, which the pattern test already guards.
	fake.Get(containerID).Stdout("[Info   :   BepInEx] 1 plugin to load\nGame server connected\n")

	got := startInstance(t, rt, admin)
	if got.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded", got)
	}
	if log := jobLog(t, db, got.JobID); strings.Contains(log, "BepInEx never reported") {
		t.Errorf("a server that did load plugins was warned about:\n%s", log)
	}
}

// TestAVanillaInstanceIsNotAskedAboutPlugins: a server with no BepInEx has no plugin count
// to report, and warning about it would train operators to ignore the warning.
func TestAVanillaInstanceIsNotAskedAboutPlugins(t *testing.T) {
	shrinkPluginWindow(t)
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopped")
	fake.Get(containerID).Stdout("Game server connected\n")

	got := startInstance(t, rt, admin)
	if got.Status != "succeeded" {
		t.Fatalf("start = %+v, want succeeded", got)
	}
	if log := jobLog(t, db, got.JobID); strings.Contains(log, "BepInEx") {
		t.Errorf("a vanilla instance was warned about BepInEx:\n%s", log)
	}
}

// TestModdedIsNotSetWhenTheInstallDoesNotPlaceBepInEx: modded is a fact about the
// filesystem, so a mod installed onto an already-modded server must not re-assert it, and
// an install that places no framework must not assert it at all.
func TestModdedIsNotSetWhenTheInstallDoesNotPlaceBepInEx(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, aPlainMod())
	seed(t, db, `UPDATE instances SET modded = TRUE WHERE id = 'inst-a'`)

	installOK(t, rt, admin, "Smoothbrain-Sailing", "1.1.8")

	if got := instanceRow(t, db).BepInExVersion; got != nil {
		t.Errorf("bepinex_version = %q; this closure placed no framework package", *got)
	}
	var manifest []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(
		[]byte(installedRows(t, db)["Smoothbrain-Sailing"].FileManifest),
		&manifest,
	); err != nil {
		t.Fatal(err)
	}
	if len(manifest) == 0 {
		t.Error("the mod recorded no files")
	}
}

// TestResolvePreviewsTheAutoInstalledBepInEx is 04 §3's contract: the dry run is what the
// user confirms before anything downloads, so it has to name every package the install
// would place — the framework the panel adds on their behalf included.
func TestResolvePreviewsTheAutoInstalledBepInEx(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t,
		aPlainMod(),
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
	)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, installBody("Smoothbrain-Sailing", "1.1.8"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var got struct {
		Nodes []struct {
			FullName   string `json:"full_name"`
			Transitive bool   `json:"transitive"`
		} `json:"nodes"`
	}
	decodeInto(t, rec, &got)
	if len(got.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want the mod and the framework the install would add", got.Nodes)
	}
	for _, n := range got.Nodes {
		if n.FullName == BepInExPack && !n.Transitive {
			t.Error("the auto-installed framework must be marked transitive; the user did not ask for it")
		}
	}
}

// TestConsoleLoggingIsFlippedAfterTheInstallCommits. The .cfg edit is in no manifest —
// an install never overwrites an existing config — so doing it inside the job would leave a
// byte the rollback cannot undo. It runs after the job commits, which also means a rolled
// back install leaves the operator's own value alone.
func TestConsoleLoggingIsFlippedAfterTheInstallCommits(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t,
		aPlainMod(),
		modPackageFixture{fullName: BepInExPack, version: "5.4.2333", files: bepinexZip()},
	)
	const operators = "[Logging.Console]\nEnabled = false\n"
	writeServerFile(t, dataDir, "BepInEx/config/BepInEx.cfg", operators)

	installOK(t, rt, admin, "Smoothbrain-Sailing", "1.1.8")

	// AfterFinish runs once the Finish transaction has committed, so it may land just after
	// the job row goes terminal.
	path := filepath.Join(dataDir, "server", "BepInEx", "config", "BepInEx.cfg")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(body), "Enabled = true") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, _ := os.ReadFile(path)
	t.Errorf("BepInEx.cfg = %q, want the console key turned on", body)
	_ = db
}
