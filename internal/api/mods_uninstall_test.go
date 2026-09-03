package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

func deleteMod(t *testing.T, rt *Router, u *store.User, fullName, query string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodDelete, "/api/v1/instances/inst-a/mods/"+fullName+query, http.NoBody))
}

func patchMod(
	t *testing.T,
	rt *Router,
	u *store.User,
	fullName string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a/mods/"+fullName, jsonBody(t, body)))
}

// installClosure installs a package and waits for the job, so a test about uninstalling has
// something to uninstall without repeating the install assertions.
func installClosure(t *testing.T, rt *Router, u *store.User, fullName, version string) {
	t.Helper()
	rec := postInstall(t, rt, u, fullName, version)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, u, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("install job = %+v, want succeeded", got)
	}
}

// TestUninstallReturnsTheTreeToWhereItWas asserts that install then uninstall leaves
// server/ byte-identical, over a three-deep
// closure whose files land in a shared BepInEx/plugins/ as well as at the server root.
func TestUninstallReturnsTheTreeToWhereItWas(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	// An operator file that predates the install: the uninstall must return the tree to
	// *this*, not to empty.
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game")
	writeServerFile(t, dataDir, "BepInEx/config/Operator.cfg", "written by hand")
	before := serverTree(t, dataDir)

	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if got := serverTree(t, dataDir); got == before {
		t.Fatal("the install changed nothing; the rest of this test proves nothing")
	}

	rec := deleteMod(t, rt, admin, "OdinPlus-OdinArchitect", "?remove_orphans=true")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstall job = %+v, want succeeded", got)
	}

	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after uninstall:\n%s\nbefore install:\n%s", got, before)
	}
	if rows := installedRows(t, db); len(rows) != 0 {
		t.Errorf("instance_mods = %+v, want none", rows)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.RestartRequired {
		t.Error("restart_required is not set after an uninstall (ADR-012)")
	}
	// The framework package went with the orphans, so the instance is a vanilla server
	// again — and E1's plugin assertion must stop applying to it.
	if inst.Modded {
		t.Error("the instance is still marked modded after BepInEx was removed")
	}
}

// TestUninstallRefusesWhileAnotherModNeedsIt covers the dependent case. Removing a
// dependency out from under a dependent leaves the dependent installed and unloadable,
// which reads to the admin as the mod breaking rather than the panel breaking it.
func TestUninstallRefusesWhileAnotherModNeedsIt(t *testing.T) {
	pkgs := []modPackageFixture{
		{
			fullName: "Therzie-Armory", version: "1.4.0",
			deps:  []string{"Therzie-Warfare-1.5.0"},
			files: map[string]string{"manifest.json": "{}", "Armory.dll": "armory"},
		},
		{
			fullName: "Therzie-Warfare", version: "1.5.0",
			files: map[string]string{"manifest.json": "{}", "Warfare.dll": "warfare"},
		},
	}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "Therzie-Armory", "1.4.0")
	before := serverTree(t, dataDir)

	rec := deleteMod(t, rt, admin, "Therzie-Warfare", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "mod_conflict" {
		t.Errorf("code = %q, want mod_conflict", got)
	}
	if !strings.Contains(rec.Body.String(), "Therzie-Armory") {
		t.Errorf("the refusal does not name the dependent: %s", rec.Body)
	}
	if got := serverTree(t, dataDir); got != before {
		t.Error("the refused uninstall still touched server/")
	}
	if rows := installedRows(t, db); len(rows) != 2 {
		t.Errorf("instance_mods = %+v, want both rows intact", rows)
	}

	// Removing the dependent first frees it, and the orphan is offered rather than taken:
	// without remove_orphans, Warfare stays behind as an explicit decision.
	var accepted jobView
	decodeInto(t, deleteMod(t, rt, admin, "Therzie-Armory", ""), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstalling the dependent = %+v, want succeeded", got)
	}
	rows := installedRows(t, db)
	if _, ok := rows["Therzie-Warfare"]; !ok || len(rows) != 1 {
		t.Fatalf("instance_mods = %+v, want Warfare alone", rows)
	}
	if _, err := os.Stat(serverPath(dataDir, "BepInEx/plugins/Therzie-Warfare/Warfare.dll")); err != nil {
		t.Errorf("the orphan's files were removed without being asked for: %v", err)
	}
}

// TestUninstallKeepsAConfigItDidNotWrite covers an admin's own config. The file was never
// in the manifest — install skipped it, because 03 §6.4 never overwrites a config — so
// uninstall cannot remove it. B9 and B10 meeting at the same file.
func TestUninstallKeepsAConfigItDidNotWrite(t *testing.T) {
	pkgs := []modPackageFixture{{
		fullName: "Ns-Configured", version: "1.0.0",
		files: map[string]string{
			"manifest.json":          "{}",
			"plugins/Configured.dll": "dll",
			"config/Configured.cfg":  "the package's default",
		},
	}}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)
	writeServerFile(t, dataDir, "BepInEx/config/Configured.cfg", "the admin's settings")

	installClosure(t, rt, admin, "Ns-Configured", "1.0.0")
	var accepted jobView
	decodeInto(t, deleteMod(t, rt, admin, "Ns-Configured", ""), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstall = %+v, want succeeded", got)
	}

	body, err := os.ReadFile(serverPath(dataDir, "BepInEx/config/Configured.cfg"))
	if err != nil {
		t.Fatalf("the admin's config was removed by the uninstall: %v", err)
	}
	if string(body) != "the admin's settings" {
		t.Errorf("config = %q, want the admin's own bytes", body)
	}
}

// TestUninstallReadsNoPlacementHeuristic asserts uninstall is manifest-driven, in the
// strongest form available: the package is gone from the index and its zip from the cache, so no
// heuristic could be re-run even if the code wanted to — and a file sitting in the
// package's own directory that the manifest does not name survives, which a heuristic
// re-run (delete the directory the rules say this package owns) would have taken.
func TestUninstallReadsNoPlacementHeuristic(t *testing.T) {
	pkgs := []modPackageFixture{{
		fullName: "Ns-Loose", version: "1.0.0",
		files: map[string]string{"manifest.json": "{}", "Loose.dll": "loose"},
	}}
	rt, db, admin, _, dataDir := installWorld(t, pkgs...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "Ns-Loose", "1.0.0")

	writeServerFile(t, dataDir, "BepInEx/plugins/Ns-Loose/Notes.txt", "left by the admin")
	seed(t, db, `DELETE FROM mod_versions`)
	seed(t, db, `DELETE FROM mod_packages`)
	if err := os.RemoveAll(filepath.Join(rt.Supervisor().inst.Cfg.Data.Root, "cache")); err != nil {
		t.Fatal(err)
	}

	var accepted jobView
	decodeInto(t, deleteMod(t, rt, admin, "Ns-Loose", ""), &accepted)
	if got := waitJob(t, rt, admin, accepted.JobID); got.Status != "succeeded" {
		t.Fatalf("uninstall = %+v, want succeeded", got)
	}
	if _, err := os.Stat(serverPath(dataDir, "BepInEx/plugins/Ns-Loose/Loose.dll")); err == nil {
		t.Error("the package's own file survived the uninstall")
	}
	body, err := os.ReadFile(serverPath(dataDir, "BepInEx/plugins/Ns-Loose/Notes.txt"))
	if err != nil || string(body) != "left by the admin" {
		t.Errorf("a file the manifest does not name was removed: %q, %v", body, err)
	}
}

// TestUninstallRestoresWhatItRemovedWhenOneFileWillNotGo is 12 §9.4's "not resumed — rolls
// back", from the inside: the job saves every file before it removes any, so a package that
// will not come off leaves the ones already removed put back rather than a server missing
// half a closure.
func TestUninstallRestoresWhatItRemovedWhenOneFileWillNotGo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the removal cannot be made to fail")
	}
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	after := serverTree(t, dataDir)

	// The BepInEx pack is removed after the two plugins, and a read-only parent directory
	// makes exactly one of its files undeletable while leaving it readable — so the backup
	// pass succeeds and the removal is what fails.
	locked := serverPath(dataDir, "doorstop_libs")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var accepted jobView
	decodeInto(t, deleteMod(t, rt, admin, "OdinPlus-OdinArchitect", "?remove_orphans=true"), &accepted)
	got := waitJob(t, rt, admin, accepted.JobID)
	if got.Status != "failed" {
		t.Fatalf("uninstall = %+v, want failed", got)
	}
	// The job must have failed *while removing*, not while saving: a failure in the backup
	// pass removes nothing, and every assertion below would then hold for the wrong reason.
	if got.Progress < 60 {
		t.Fatalf("the job failed at progress %d, before it began removing anything", got.Progress)
	}
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if tree := serverTree(t, dataDir); tree != after {
		t.Errorf("the failed uninstall did not put the tree back:\n%s\nwant:\n%s", tree, after)
	}
	if rows := installedRows(t, db); len(rows) != 3 {
		t.Errorf("instance_mods = %+v, want all three rows still there", rows)
	}
}

// TestUninstallAgainstARunningInstanceIsRefused is B11 and C19 on the removal path: a job
// never implicitly stops a server, and BepInEx only reads the plugin directory at startup
// anyway.
func TestUninstallAgainstARunningInstanceIsRefused(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	after := serverTree(t, dataDir)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)

	rec := deleteMod(t, rt, admin, "OdinPlus-OdinArchitect", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "instance_must_be_stopped" {
		t.Errorf("code = %q, want instance_must_be_stopped", got)
	}
	if tree := serverTree(t, dataDir); tree != after {
		t.Error("the refused uninstall still touched server/")
	}
}

// TestUninstallOfSomethingNotInstalledIs404 is D2 / ADR-038: a resource that is not there
// reads as absent, not as forbidden or as a validation failure.
func TestUninstallOfSomethingNotInstalledIs404(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	alreadyModded(t, db)

	if rec := deleteMod(t, rt, admin, "Ns-Absent", ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestModUninstallCancelPolicy is 12 §3.1's row: not cancellable, at any checkpoint. Half a
// package removed is not an outcome anyone asked for.
func TestModUninstallCancelPolicy(t *testing.T) {
	for _, checkpoint := range []string{"", checkpointSaved, checkpointRemoved} {
		if ok, phase := modUninstallCancelPolicy(checkpoint); ok || phase == "" {
			t.Errorf("modUninstallCancelPolicy(%q) = %v, %q; want false and a named phase",
				checkpoint, ok, phase)
		}
	}
}

// TestPatchTagsAMod is 03 §5.6: the admin says what a mod is for, because Thunderstore
// metadata does not, and a fresh install is therefore `unknown`.
func TestPatchTagsAMod(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if got := installedRows(t, db)["OdinPlus-OdinArchitect"].Side; got != store.SideUnknown {
		t.Fatalf("a fresh install's side = %q, want unknown", got)
	}

	rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect", map[string]any{"side": "client_required"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var view installedModView
	decodeInto(t, rec, &view)
	if view.Side != "client_required" {
		t.Errorf("response side = %q, want client_required", view.Side)
	}
	rows := installedRows(t, db)
	if got := rows["OdinPlus-OdinArchitect"].Side; got != "client_required" {
		t.Errorf("stored side = %q, want client_required", got)
	}
	// The other rows are untouched — a PATCH names one mod.
	if got := rows["ValheimModding-Jotunn"].Side; got != store.SideUnknown {
		t.Errorf("a mod nobody patched has side %q", got)
	}
	// enabled is independent of side: patching one must not blank the other.
	if !rows["OdinPlus-OdinArchitect"].Enabled {
		t.Error("patching side turned the mod off")
	}
	if rec := patchMod(
		t,
		rt,
		admin,
		"OdinPlus-OdinArchitect",
		map[string]any{"enabled": false},
	); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rows = installedRows(t, db)
	if rows["OdinPlus-OdinArchitect"].Enabled {
		t.Error("enabled was not stored")
	}
	if got := rows["OdinPlus-OdinArchitect"].Side; got != "client_required" {
		t.Errorf("patching enabled reset side to %q", got)
	}
}

// TestPatchRejectsWhatItCannotStore. The four values are 04 §2's CHECK constraint, and an
// empty body is a request that says nothing — both are answered by the API rather than by a
// constraint violation surfacing as a 500.
func TestPatchRejectsWhatItCannotStore(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")

	for _, body := range []map[string]any{
		{"side": "server-only"},
		{"side": ""},
		{},
	} {
		rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PATCH %v = %d, want 422 (%s)", body, rec.Code, rec.Body)
		}
	}
	if rec := patchMod(
		t,
		rt,
		admin,
		"Ns-Absent",
		map[string]any{"side": "server_only"},
	); rec.Code != http.StatusNotFound {
		t.Errorf("patching a mod that is not installed = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if got := installedRows(t, db)["OdinPlus-OdinArchitect"].Side; got != store.SideUnknown {
		t.Errorf("a rejected PATCH still changed side to %q", got)
	}
}

// TestModRoutesAreRefusedToAMemberWithoutAGrant is D1 and D2 on the two new routes: the
// instance is invisible to this user, so both answer 404 rather than 403 (ADR-038).
func TestModRoutesAreRefusedToAMemberWithoutAGrant(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	seed(t, db, `DELETE FROM instance_grants WHERE user_id = 'u-member'`)

	if rec := deleteMod(t, rt, member, "OdinPlus-OdinArchitect", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE as a member = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := patchMod(
		t,
		rt,
		member,
		"OdinPlus-OdinArchitect",
		map[string]any{"side": "server_only"},
	); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH as a member = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// writeBackupFile plants a file in a job's staging backup area, as the job itself would
// have before it removed or replaced the original.
func writeBackupFile(t *testing.T, staging, rel, body string) {
	t.Helper()
	p := filepath.Join(staging, "backup", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheSweepRestoresAnInterruptedUninstall is 12 §9.4 for mod_uninstall: not resumed,
// rolled back. The rows are deleted only in the job's Finish transaction, so a panel killed
// mid-removal still has them — and putting the files back from what the job saved is the
// whole of the recovery.
func TestTheSweepRestoresAnInterruptedUninstall(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)

	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	writeServerFile(t, dataDir, "BepInEx/plugins/Half.dll", "installed")
	writeServerFile(t, dataDir, "BepInEx/plugins/Also.dll", "installed too")
	before := serverTree(t, dataDir)

	root := modStagingRoot(rt.Supervisor().inst.Cfg.Data.Root)
	staging, err := os.MkdirTemp(mkdirAllT(t, root), "uninstall-*")
	if err != nil {
		t.Fatal(err)
	}
	// What the killed job had done: saved both files, then removed one of them.
	writeBackupFile(t, staging, "BepInEx/plugins/Half.dll", "installed")
	writeBackupFile(t, staging, "BepInEx/plugins/Also.dll", "installed too")
	if err := os.Remove(serverPath(dataDir, "BepInEx/plugins/Half.dll")); err != nil {
		t.Fatal(err)
	}

	manifest, err := json.Marshal([]installer.ManifestEntry{
		{Path: "BepInEx/plugins/Half.dll"}, {Path: "BepInEx/plugins/Also.dll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteInstanceMods(t.Context(), "inst-a", []store.InstanceMod{{
		InstanceID: "inst-a", FullName: "Ns-Half", Version: "1.0.0",
		InstalledAs: store.InstalledExplicit, Side: store.SideUnknown, Enabled: true,
		FileManifest: string(manifest),
	}}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(modUninstallPayload{StagingDir: staging, FullNames: []string{"Ns-Half"}})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_uninstall", checkpointSaved, string(payload))

	if _, err := rt.Supervisor().sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after the sweep:\n%s\nwant:\n%s", got, before)
	}
	if rows := installedRows(t, db); len(rows) != 1 {
		t.Errorf("instance_mods = %+v, want the row of the mod that is back on disk", rows)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("the staging directory survived the sweep: %v", err)
	}
}

// TestTheSweepRefusesAnUninstallStagingPathOutsideTheStagingRoot is B5 on the second
// recovery path, for the same reason as the first: staging_dir comes out of a database
// column and feeds a recursive delete.
func TestTheSweepRefusesAnUninstallStagingPathOutsideTheStagingRoot(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	elsewhere := t.TempDir()

	payload, err := json.Marshal(modUninstallPayload{StagingDir: elsewhere, FullNames: []string{"Ns-X"}})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_uninstall", checkpointSaved, string(payload))

	if _, err := rt.Supervisor().sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("the sweep removed a directory outside the staging root: %v", err)
	}
}

// listMods is GET /instances/{id}/mods with the load-verification half of the response.
func listMods(t *testing.T, rt *Router, u *store.User) (
	mods map[string]installedModView, load *pluginLoadView,
) {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Mods       []installedModView `json:"mods"`
		PluginLoad *pluginLoadView    `json:"plugin_load"`
	}
	decodeInto(t, rec, &body)
	mods = map[string]installedModView{}
	for i := range body.Mods {
		mods[body.Mods[i].FullName] = body.Mods[i]
	}
	return mods, body.PluginLoad
}

func statusOf(t *testing.T, mods map[string]installedModView, fullName string) string {
	t.Helper()
	m, ok := mods[fullName]
	if !ok {
		t.Fatalf("%s is not in the list", fullName)
	}
	if m.LoadStatus == nil {
		return "null"
	}
	return *m.LoadStatus
}

// TestLoadVerificationReportsWhatBootedAndWhatDidNot covers load verification on the route
// that carries it: a plugin BepInEx named reports `loaded`, one
// installed on disk that no line names reports `not_seen`, and the instance is untouched by
// either — 12 §2's "mods failed to load" is a warning about a server that is up, never a
// state change (ADR-043).
func TestLoadVerificationReportsWhatBootedAndWhatDidNot(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")

	// Before any boot there is no chainloader run to compare against, and saying "not
	// loading" about a server that has never been started is the false alarm the null case
	// exists to prevent.
	mods, load := listMods(t, rt, admin)
	if load != nil {
		t.Errorf("plugin_load = %+v before the server has ever booted", load)
	}
	for _, name := range []string{"OdinPlus-OdinArchitect", "ValheimModding-Jotunn"} {
		if got := statusOf(t, mods, name); got != "null" {
			t.Errorf("%s load_status = %q before a boot; want null", name, got)
		}
	}

	// One plugin, singular in the count line (E9): Jotunn loaded, OdinArchitect did not.
	writeServerFile(t, dataDir, "BepInEx/LogOutput.log",
		"[Message:   BepInEx] Chainloader started\n"+
			"[Info   :   BepInEx] 1 plugin to load\n"+
			"[Info   :   BepInEx] Loading [Jotunn 2.29.2]\n"+
			"[Message:   BepInEx] Chainloader startup complete\n")
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)

	mods, load = listMods(t, rt, admin)
	if got := statusOf(t, mods, "ValheimModding-Jotunn"); got != LoadLoaded {
		t.Errorf("Jotunn load_status = %q, want loaded", got)
	}
	if got := statusOf(t, mods, "OdinPlus-OdinArchitect"); got != LoadNotSeen {
		t.Errorf("OdinArchitect load_status = %q, want not_seen — it is on disk and no line named it", got)
	}
	// The framework package is never named by a Loading line; reporting it not_seen would be
	// a permanent warning about the thing that makes mods work at all.
	if got := statusOf(t, mods, BepInExPack); got != "null" {
		t.Errorf("the BepInEx pack load_status = %q, want null", got)
	}
	if load == nil {
		t.Fatal("plugin_load is null after a boot that printed a chainloader run")
	}
	if load.Declared == nil || *load.Declared != 1 || load.Loaded != 1 {
		t.Errorf("plugin_load = %+v, want 1 declared and 1 named", load)
	}
	if load.Discrepancy != nil {
		t.Errorf("discrepancy = %q; one declared and one named agree", *load.Discrepancy)
	}
	if load.ObservedAt == "" {
		t.Error("observed_at is empty; the boot the result came from is not identified")
	}

	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "running" {
		t.Errorf("state = %q; a mod that did not load never moves the instance (12 §2)", inst.State)
	}
}

// TestLoadVerificationSurfacesADiscrepancy asserts a count mismatch is surfaced: BepInEx saying
// it will load two and naming one is reported as the disagreement it is, not resolved
// toward either number.
func TestLoadVerificationSurfacesADiscrepancy(t *testing.T) {
	rt, _, admin, _, dataDir := installWorld(t, threeDeep()...)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	writeServerFile(t, dataDir, "BepInEx/LogOutput.log",
		"[Info   :   BepInEx] 2 plugins to load\n"+
			"[Info   :   BepInEx] Loading [Jotunn 2.29.2]\n")

	_, load := listMods(t, rt, admin)
	if load == nil || load.Discrepancy == nil {
		t.Fatalf("plugin_load = %+v; the count line and the plugin lines disagree", load)
	}
	if load.Declared == nil || *load.Declared != 2 || load.Loaded != 1 {
		t.Errorf("plugin_load = %+v; both numbers must survive the disagreement", load)
	}
}
