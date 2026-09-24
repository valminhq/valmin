package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// toggleMod disables or enables a mod and waits for the job.
func toggleMod(t *testing.T, rt *Router, u *store.User, fullName string, enable bool) {
	t.Helper()
	rec := patchMod(t, rt, u, fullName, map[string]any{"enabled": enable})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("toggle %s to %v = %d (%s), want 202", fullName, enable, rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, u, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("toggle job = %+v, want succeeded", got)
	}
}

// parkedPath is where a disabled package's file waits.
func parkedPath(dataDir, fullName, rel string) string {
	return filepath.Join(instance.ParkedModsDir(dataDir), fullName, filepath.FromSlash(rel))
}

func manifestFor(t *testing.T, db *store.DB, fullName string) []installer.ManifestEntry {
	t.Helper()
	var manifest []installer.ManifestEntry
	if err := json.Unmarshal([]byte(installedRows(t, db)[fullName].FileManifest), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

const odinDLL = "BepInEx/plugins/OdinArchitect.dll"

// TestDisablingMovesTheModOutAndEnablingPutsItBack is Q37: disabling is real, so the plugin
// is no longer where BepInEx loads from, and enabling returns the tree to exactly what it was.
func TestDisablingMovesTheModOutAndEnablingPutsItBack(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if !exists(serverPath(dataDir, odinDLL)) {
		t.Fatalf("fixture: %s was not installed where this test expects", odinDLL)
	}
	before := serverTree(t, dataDir)
	seed(t, db, `UPDATE instances SET restart_required = FALSE WHERE id = 'inst-a'`)

	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", false)

	if exists(serverPath(dataDir, odinDLL)) {
		t.Error("the disabled plugin is still where BepInEx would load it")
	}
	if !exists(parkedPath(dataDir, "OdinPlus-OdinArchitect", odinDLL)) {
		t.Error("the disabled plugin is not in the parking tree")
	}
	row := installedRows(t, db)["OdinPlus-OdinArchitect"]
	if row.Enabled {
		t.Error("the row still says enabled")
	}
	if parked := installer.ParkedPaths(manifestFor(t, db, "OdinPlus-OdinArchitect")); len(parked) == 0 {
		t.Error("the manifest does not record the move")
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.RestartRequired {
		t.Error("a disable did not mark the server as needing a restart")
	}
	if mods, _ := listMods(t, rt, admin); mods["OdinPlus-OdinArchitect"].Enabled {
		t.Error("the list reports the mod enabled")
	}

	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", true)

	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after disable and enable:\n%s\nwant:\n%s", got, before)
	}
	if exists(filepath.Join(instance.ParkedModsDir(dataDir), "OdinPlus-OdinArchitect")) {
		t.Error("the parking directory outlived the enable")
	}
	if parked := installer.ParkedPaths(manifestFor(t, db, "OdinPlus-OdinArchitect")); len(parked) != 0 {
		t.Errorf("the manifest still records parked paths %v", parked)
	}
	if !installedRows(t, db)["OdinPlus-OdinArchitect"].Enabled {
		t.Error("the row still says disabled")
	}
}

// TestADisabledModCanStillBeUninstalledExactly is B9 across the move: the manifest says where
// each file is, and uninstall removes exactly those, from both trees.
func TestADisabledModCanStillBeUninstalledExactly(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", false)
	jotunn := serverPath(dataDir, "BepInEx/plugins/Jotunn.dll")
	if !exists(jotunn) {
		t.Fatal("fixture: Jotunn is not where this test expects")
	}

	rec := deleteMod(t, rt, admin, "OdinPlus-OdinArchitect", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("uninstall = %d (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstall job = %+v, want succeeded", got)
	}

	if exists(parkedPath(dataDir, "OdinPlus-OdinArchitect", odinDLL)) || exists(serverPath(dataDir, odinDLL)) {
		t.Error("the disabled mod's plugin survived its uninstall")
	}
	if exists(filepath.Join(instance.ParkedModsDir(dataDir), "OdinPlus-OdinArchitect")) {
		t.Error("the parking directory survived the uninstall")
	}
	if _, ok := installedRows(t, db)["OdinPlus-OdinArchitect"]; ok {
		t.Error("the row survived the uninstall")
	}
	if !exists(jotunn) {
		t.Error("uninstalling one mod removed another's file")
	}
}

// TestTogglingRespectsDependencies asserts neither direction can leave an enabled mod that
// BepInEx cannot load: no disabling what an enabled mod needs, no enabling over a disabled
// dependency, and never the mod loader itself.
func TestTogglingRespectsDependencies(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")

	conflict := func(fullName string, enable bool, detail string) {
		t.Helper()
		rec := patchMod(t, rt, admin, fullName, map[string]any{"enabled": enable})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), detail) {
			t.Errorf("toggle %s to %v = %d (%s), want 409 naming %s",
				fullName, enable, rec.Code, rec.Body, detail)
		}
	}
	conflict("ValheimModding-Jotunn", false, "OdinPlus-OdinArchitect")
	conflict(BepInExPack, false, "mod loader")

	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", false)
	toggleMod(t, rt, admin, "ValheimModding-Jotunn", false)
	conflict("OdinPlus-OdinArchitect", true, "ValheimModding-Jotunn")
	toggleMod(t, rt, admin, "ValheimModding-Jotunn", true)
	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", true)
}

// TestTogglingNeedsAStoppedServer is B11 for the new file mover.
func TestTogglingNeedsAStoppedServer(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)

	rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect", map[string]any{"enabled": false})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "instance_must_be_stopped") {
		t.Errorf("disable on a running server = %d (%s), want 409 instance_must_be_stopped",
			rec.Code, rec.Body)
	}
}

// TestEnabledAndSideAreSentSeparately asserts a request cannot be a label edit and a job at once.
func TestEnabledAndSideAreSentSeparately(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect",
		map[string]any{"enabled": false, "side": "server_only"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("mixed patch = %d (%s), want 422", rec.Code, rec.Body)
	}
}

// TestAnUpdateTouchingADisabledModIsRefused asserts install and update refuse a parked package:
// updating it would place files beside the ones it parked.
func TestAnUpdateTouchingADisabledModIsRefused(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "Ns-Only", "1.0.0")
	toggleMod(t, rt, admin, "Ns-Only", false)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, installBody("Ns-Only", "2.0.0"))))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "disabled") {
		t.Errorf("resolve over a disabled mod = %d (%s), want 409 naming it", rec.Code, rec.Body)
	}

	var accepted jobView
	decodeInto(t, postInstall(t, rt, admin, "Ns-Only", "2.0.0"), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "failed" ||
		got.ErrorCode == nil || *got.ErrorCode != "mod_conflict" {
		t.Errorf("install over a disabled mod = %+v, want failed with mod_conflict", got)
	}
	if got := installedRows(t, db)["Ns-Only"].Version; got != "1.0.0" {
		t.Errorf("the disabled mod moved to %s", got)
	}
	if preview := previewUpdatesOf(t, rt, admin); len(preview.Targets) != 0 {
		t.Errorf("update-all offers the disabled mod: %+v", preview.Targets)
	}
}

// TestAGameUpdateReplaysOnlyWhatIsInTheServer asserts the replay onto a fresh server leaves a
// disabled mod's parked files out: they live beside server/, which the swap does not touch.
func TestAGameUpdateReplaysOnlyWhatIsInTheServer(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", false)
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "replay")
	if err := rt.mods.StageReplay(t.Context(), inst, dest); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dest, filepath.FromSlash(odinDLL))) {
		t.Error("the replay put a disabled plugin back into the server")
	}
	if !exists(filepath.Join(dest, "BepInEx", "plugins", "Jotunn.dll")) {
		t.Error("the replay dropped an enabled mod")
	}
}

// TestTheClientExportLeavesOutADisabledMod asserts players are not told to install a mod the
// server does not run.
func TestTheClientExportLeavesOutADisabledMod(t *testing.T) {
	rt, _, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect",
		map[string]any{"side": "client_required"}); rec.Code != http.StatusOK {
		t.Fatalf("tag = %d", rec.Code)
	}
	toggleMod(t, rt, admin, "OdinPlus-OdinArchitect", false)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods/export", http.NoBody))
	var preview exportPreview
	decodeInto(t, rec, &preview)
	for _, m := range preview.Mods {
		if m.FullName == "OdinPlus-OdinArchitect" {
			t.Error("the export includes a disabled mod")
		}
	}
	found := false
	for _, m := range preview.Excluded {
		if m.FullName == "OdinPlus-OdinArchitect" {
			found = m.Reason == reasonDisabled
		}
	}
	if !found {
		t.Errorf("excluded = %+v, want the disabled mod with reason disabled", preview.Excluded)
	}
}

// TestTheSweepSettlesAnInterruptedDisable is 12 §9.4 for mod_toggle: the row is only written in
// the Finish transaction, so a crash leaves it describing the tree before the job, and the sweep
// returns each file there from whichever tree it was left in.
func TestTheSweepSettlesAnInterruptedDisable(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	before := serverTree(t, dataDir)

	// What the killed job had done: parked the plugin, and never recorded it.
	parked := parkedPath(dataDir, "OdinPlus-OdinArchitect", odinDLL)
	body, err := os.ReadFile(serverPath(dataDir, odinDLL))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(parked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parked, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(serverPath(dataDir, odinDLL)); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(modTogglePayload{FullName: "OdinPlus-OdinArchitect"})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_toggle", "", string(payload))

	if _, err := rt.Supervisor().sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after the sweep:\n%s\nwant:\n%s", got, before)
	}
	if exists(parked) {
		t.Error("the parked copy survived the sweep")
	}
}

// TestACloneCarriesTheParkingTree asserts a cloned instance can enable what its source had
// disabled: the copied row says the files are parked, so the parked files come too.
func TestACloneCarriesTheParkingTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	rel := filepath.Join("Ns-Mod", "BepInEx", "plugins", "Mod.dll")
	p := filepath.Join(instance.ParkedModsDir(src), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("parked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cloneParkedMods(src, dst); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(instance.ParkedModsDir(dst), rel))
	if err != nil || string(body) != "parked" {
		t.Errorf("cloned parked file = %q, %v", body, err)
	}
	if err := cloneParkedMods(t.TempDir(), t.TempDir()); err != nil {
		t.Errorf("a source with nothing parked: %v", err)
	}
}
