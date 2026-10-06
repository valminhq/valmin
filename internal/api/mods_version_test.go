package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// resolvePreview posts a resolve and decodes the 200 it must answer.
func resolvePreview(t *testing.T, rt *Server, u *store.User, fullName, version string) resolveResponse {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody(fullName, version))))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve %s-%s = %d (%s)", fullName, version, rec.Code, rec.Body)
	}
	var got resolveResponse
	decodeInto(t, rec, &got)
	return got
}

func previewNode(t *testing.T, nodes []resolvedNode, fullName string) resolvedNode {
	t.Helper()
	for _, n := range nodes {
		if n.FullName == fullName {
			return n
		}
	}
	t.Fatalf("%s not in the preview: %+v", fullName, nodes)
	return resolvedNode{}
}

// installFails posts an install and waits for the job, which must fail with code.
func installFails(t *testing.T, rt *Server, u *store.User, fullName, version, code string) jobView {
	t.Helper()
	var accepted jobView
	decodeInto(t, postInstall(t, rt, u, fullName, version), &accepted)
	got := waitJob(t, rt, u, accepted.JobID)
	if got.Status != "failed" || got.ErrorCode == nil || *got.ErrorCode != code {
		t.Fatalf("install of %s-%s = %+v, want failed with %s", fullName, version, got, code)
	}
	return got
}

// assertServerFiles checks each path's contents, an empty want meaning the file must be gone.
func assertServerFiles(t *testing.T, dataDir string, want map[string]string) {
	t.Helper()
	for path, body := range want {
		got, err := os.ReadFile(serverPath(dataDir, path))
		switch {
		case body == "" && err == nil:
			t.Errorf("%s is still there: %q", path, got)
		case body == "":
		case err != nil:
			t.Errorf("%s: %v", path, err)
		case string(got) != body:
			t.Errorf("%s = %q, want %q", path, got, body)
		}
	}
}

// TestADowngradeInstallsTheRequestedVersion asserts a version below the installed one is
// honoured, previewed as a downgrade, backed up first, and placed from its own manifest.
func TestADowngradeInstallsTheRequestedVersion(t *testing.T) {
	rt, db, admin, _, dataDir := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Only", "2.0.0")
	giveWorld(t, dataDir)

	preview := resolvePreview(t, rt, admin, "Ns-Only", "1.0.0")
	n := previewNode(t, preview.Nodes, "Ns-Only")
	if n.Change != changeDowngrade || n.FromVersion != "2.0.0" || n.Version != "1.0.0" || n.NoOp {
		t.Errorf("node = %+v, want a downgrade from 2.0.0 to 1.0.0", n)
	}
	if !preview.Backup || len(preview.Conflicts) != 0 {
		t.Errorf("preview = %+v, want a backup and no conflicts", preview)
	}

	installOK(t, rt, admin, "Ns-Only", "1.0.0")
	assertServerFiles(t, dataDir, map[string]string{
		"BepInEx/plugins/Only.dll": "v1",
		"BepInEx/plugins/Gone.dll": "removed in v2",
		"BepInEx/plugins/New.dll":  "",
	})
	if row := installedRows(t, db)["Ns-Only"]; row.Version != "1.0.0" {
		t.Errorf("row = %+v, want 1.0.0", row)
	}
	if got := preUpdateArchives(t, db); got != 1 {
		t.Errorf("pre_update archives = %d, want 1", got)
	}
}

// TestADowngradeThatBreaksAnInstalledModIsAConflict asserts a downgrade below what another
// installed mod needs is previewed as a conflict and refused by the job, changing nothing.
func TestADowngradeThatBreaksAnInstalledModIsAConflict(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, updatable()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Other", "1.1.0")

	preview := resolvePreview(t, rt, admin, "Ns-Lib", "1.0.0")
	want := []conflictView{{
		FullName: "Ns-Other", Version: "1.1.0", Dependency: "Ns-Lib", Requires: "1.1.0", Have: "1.0.0",
	}}
	if !reflect.DeepEqual(preview.Conflicts, want) {
		t.Errorf("conflicts = %+v, want %+v", preview.Conflicts, want)
	}

	installFails(t, rt, admin, "Ns-Lib", "1.0.0", "mod_conflict")
	if row := installedRows(t, db)["Ns-Lib"]; row.Version != "1.1.0" {
		t.Errorf("Ns-Lib = %+v, want it left at 1.1.0", row)
	}
}

// TestALockedDependencyIsNeverMoved asserts a lock is audited, holds the package through an
// install that needs it raised, keeps it out of Update all, and refuses an explicit move.
func TestALockedDependencyIsNeverMoved(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, updatable()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Other", "1.0.0")

	rec := patchMod(t, rt, admin, "Ns-Lib", map[string]any{"locked": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("lock = %d (%s)", rec.Code, rec.Body)
	}
	var view installedModView
	decodeInto(t, rec, &view)
	if !view.Locked {
		t.Errorf("row = %+v, want locked", view)
	}
	locks := modAuditEntries(t, db, "instances.mods.lock")
	if want := (map[string]any{"full_name": "Ns-Lib", "version": "1.0.0"}); len(locks) != 1 ||
		!reflect.DeepEqual(locks[0].Detail, want) {
		t.Errorf("lock audit entries = %+v, want one with detail %v", locks, want)
	}

	preview := resolvePreview(t, rt, admin, "Ns-Other", "1.1.0")
	if lib := previewNode(t, preview.Nodes, "Ns-Lib"); lib.Version != "1.0.0" || !lib.NoOp {
		t.Errorf("Ns-Lib = %+v, want it held at 1.0.0", lib)
	}
	want := []conflictView{{
		FullName: "Ns-Other", Version: "1.1.0", Dependency: "Ns-Lib", Requires: "1.1.0",
		Have: "1.0.0", Locked: true,
	}}
	if !reflect.DeepEqual(preview.Conflicts, want) {
		t.Errorf("conflicts = %+v, want %+v", preview.Conflicts, want)
	}
	installFails(t, rt, admin, "Ns-Other", "1.1.0", "mod_conflict")

	updates := previewUpdatesOf(t, rt, admin)
	for _, target := range updates.Targets {
		if target.FullName == "Ns-Lib" {
			t.Errorf("Update all offers the locked Ns-Lib: %+v", updates.Targets)
		}
	}
	if len(updates.Conflicts) == 0 {
		t.Errorf("Update all preview = %+v, want the locked Ns-Lib reported as a conflict", updates)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, resolveBody("Ns-Lib", "1.1.0"))))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Locked string `json:"locked"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeInto(t, rec, &body)
	if rec.Code != http.StatusConflict || body.Error.Code != "mod_conflict" || body.Error.Details.Locked != "Ns-Lib" {
		t.Errorf("moving the locked mod = %d %s, want 409 mod_conflict naming Ns-Lib", rec.Code, rec.Body)
	}
	rows := installedRows(t, db)
	if rows["Ns-Lib"].Version != "1.0.0" || rows["Ns-Other"].Version != "1.0.0" {
		t.Errorf("rows = %+v, want both left at 1.0.0", rows)
	}
}

// TestLockAuditRecordsTheVersionHeld asserts a lock and an unlock are each audited with the
// installed version they applied to, and that locking a mod that is not installed is a 404
// which writes nothing.
func TestLockAuditRecordsTheVersionHeld(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, updatable()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Lib", "1.1.0")

	for _, locked := range []bool{true, false} {
		if rec := patchMod(t, rt, admin, "Ns-Lib", map[string]any{"locked": locked}); rec.Code != http.StatusOK {
			t.Fatalf("locked=%v = %d (%s)", locked, rec.Code, rec.Body)
		}
	}
	if rec := patchMod(t, rt, admin, "Ns-Missing", map[string]any{"locked": true}); rec.Code != http.StatusNotFound {
		t.Fatalf("locking a mod that is not installed = %d (%s), want 404", rec.Code, rec.Body)
	}

	want := map[string]any{"full_name": "Ns-Lib", "version": "1.1.0"}
	for _, action := range []string{"instances.mods.lock", "instances.mods.unlock"} {
		entries := modAuditEntries(t, db, action)
		if len(entries) != 1 {
			t.Errorf("%s entries = %+v, want one", action, entries)
			continue
		}
		if entries[0].UserID != admin.ID || entries[0].JobID != "" {
			t.Errorf("%s belongs to user %q job %q, want %q and no job",
				action, entries[0].UserID, entries[0].JobID, admin.ID)
		}
		if !reflect.DeepEqual(entries[0].Detail, want) {
			t.Errorf("%s detail = %v, want %v", action, entries[0].Detail, want)
		}
	}
}

// modpack is a pack at two versions and the mods it bundles:
//
//	Ns-Pack 1.0.0 pins Ns-A 1.0.0, Ns-B 1.0.0 and Ns-C 1.0.0
//	Ns-Pack 2.0.0 pins Ns-A 2.0.0 and Ns-B 2.0.0, and drops Ns-C
//
// Ns-B also has a 1.5.0 to pick by hand, and Ns-Solo belongs to no pack.
func modpack() []modPackageFixture {
	meta := map[string]string{"manifest.json": "{}", "README.md": "a modpack"}
	plugin := func(name, body string) map[string]string {
		return map[string]string{"manifest.json": "{}", "plugins/" + name + ".dll": body}
	}
	return []modPackageFixture{
		{fullName: "Ns-A", version: "1.0.0", files: plugin("A", "a v1")},
		{fullName: "Ns-A", version: "2.0.0", files: plugin("A", "a v2")},
		{fullName: "Ns-B", version: "1.0.0", files: plugin("B", "b v1")},
		{fullName: "Ns-B", version: "1.5.0", files: plugin("B", "b v1.5")},
		{fullName: "Ns-B", version: "2.0.0", files: plugin("B", "b v2")},
		{fullName: "Ns-C", version: "1.0.0", files: plugin("C", "c v1")},
		{fullName: "Ns-Solo", version: "1.0.0", files: plugin("Solo", "solo")},
		{fullName: "Ns-Pack", version: "1.0.0", deps: []string{"Ns-A-1.0.0", "Ns-B-1.0.0", "Ns-C-1.0.0"}, files: meta},
		{fullName: "Ns-Pack", version: "2.0.0", deps: []string{"Ns-A-2.0.0", "Ns-B-2.0.0"}, files: meta},
	}
}

// packWorld installs Ns-Pack 1.0.0 and Ns-Solo, overrides Ns-B by hand at 1.5.0, and gives the
// server a world.
func packWorld(t *testing.T, extra ...modPackageFixture) (*Server, *store.DB, *store.User, string) {
	t.Helper()
	rt, db, admin, _, dataDir := installWorld(t, append(modpack(), extra...)...)
	alreadyModded(t, db)
	seed(t, db, `UPDATE mod_packages SET categories = '["Modpacks"]' WHERE full_name = 'Ns-Pack'`)
	installOK(t, rt, admin, "Ns-Pack", "1.0.0")
	installOK(t, rt, admin, "Ns-Solo", "1.0.0")
	installOK(t, rt, admin, "Ns-B", "1.5.0")
	giveWorld(t, dataDir)
	return rt, db, admin, dataDir
}

// TestAModpackUpdateKeepsLocalOverridesAndManualMods asserts a pack update moves the members
// that follow it, keeps and reports a member changed by hand, removes a member the new version
// drops, leaves an unrelated mod alone, and that Update all leaves the pack's own members to it.
func TestAModpackUpdateKeepsLocalOverridesAndManualMods(t *testing.T) {
	rt, db, admin, dataDir := packWorld(t)

	var listed struct {
		Mods []installedModView `json:"mods"`
	}
	decodeInto(
		t,
		as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods", http.NoBody)),
		&listed,
	)
	for _, m := range listed.Mods {
		want := map[string]installedModView{
			"Ns-Pack": {IsPack: true},
			"Ns-A":    {Pack: "Ns-Pack", PackVersion: "1.0.0"},
			"Ns-B":    {Pack: "Ns-Pack", PackVersion: "1.0.0", PackOverride: true},
			"Ns-C":    {Pack: "Ns-Pack", PackVersion: "1.0.0"},
			"Ns-Solo": {},
		}[m.FullName]
		if m.IsPack != want.IsPack || m.Pack != want.Pack || m.PackVersion != want.PackVersion ||
			m.PackOverride != want.PackOverride {
			t.Errorf("%s = pack %q@%q override %v is_pack %v, want %+v",
				m.FullName, m.Pack, m.PackVersion, m.PackOverride, m.IsPack, want)
		}
	}

	updates := previewUpdatesOf(t, rt, admin)
	names := make([]string, 0, len(updates.Targets))
	for _, target := range updates.Targets {
		names = append(names, target.FullName)
	}
	if !slices.Equal(names, []string{"Ns-B"}) {
		t.Errorf("Update all targets = %v, want only the hand-picked Ns-B", names)
	}

	preview := resolvePreview(t, rt, admin, "Ns-Pack", "2.0.0")
	if n := previewNode(t, preview.Nodes, "Ns-A"); n.Change != changeUpgrade || n.Version != "2.0.0" {
		t.Errorf("Ns-A = %+v, want it to follow the pack to 2.0.0", n)
	}
	if n := previewNode(t, preview.Nodes, "Ns-B"); !n.NoOp || n.Version != "1.5.0" {
		t.Errorf("Ns-B = %+v, want it kept at 1.5.0", n)
	}
	wantKept := []keptMember{{FullName: "Ns-B", Version: "1.5.0", PackVersion: "2.0.0", Reason: keptManual}}
	if !reflect.DeepEqual(preview.Kept, wantKept) {
		t.Errorf("kept = %+v, want %+v", preview.Kept, wantKept)
	}
	wantRemovals := []removalView{{FullName: "Ns-C", Source: "thunderstore", Version: "1.0.0"}}
	if !reflect.DeepEqual(preview.Removals, wantRemovals) {
		t.Errorf("removals = %+v, want %+v", preview.Removals, wantRemovals)
	}
	if len(preview.Conflicts) != 0 || !preview.Backup {
		t.Errorf("preview = %+v, want no conflicts and a backup", preview)
	}
	for _, n := range preview.Nodes {
		if n.FullName == "Ns-Solo" {
			t.Errorf("the unrelated Ns-Solo is in the preview: %+v", n)
		}
	}

	installOK(t, rt, admin, "Ns-Pack", "2.0.0")
	rows := installedRows(t, db)
	for name, want := range map[string][2]string{
		"Ns-Pack": {"2.0.0", store.InstalledExplicit},
		"Ns-A":    {"2.0.0", store.InstalledDependency},
		"Ns-B":    {"1.5.0", store.InstalledExplicit},
		"Ns-Solo": {"1.0.0", store.InstalledExplicit},
	} {
		if row := rows[name]; row.Version != want[0] || row.InstalledAs != want[1] {
			t.Errorf("%s = %s %s, want %s %s", name, row.Version, row.InstalledAs, want[0], want[1])
		}
	}
	if _, ok := rows["Ns-C"]; ok {
		t.Error("Ns-C survived the pack update that dropped it")
	}
	assertServerFiles(t, dataDir, map[string]string{
		"BepInEx/plugins/A.dll":    "a v2",
		"BepInEx/plugins/B.dll":    "b v1.5",
		"BepInEx/plugins/C.dll":    "",
		"BepInEx/plugins/Solo.dll": "solo",
	})
	if got := preUpdateArchives(t, db); got != 1 {
		t.Errorf("pre_update archives = %d, want 1", got)
	}
}

// TestAFailedModpackUpdateRollsBackEveryPackage asserts a pack update failing part-way through
// placing files, after it has removed one member and added another, returns server/ and every
// row to where they were.
func TestAFailedModpackUpdateRollsBackEveryPackage(t *testing.T) {
	rt, db, admin, dataDir := packWorld(t,
		modPackageFixture{fullName: "Ns-D", version: "1.0.0", files: map[string]string{
			"manifest.json": "{}", "plugins/Shared": "a file, where Ns-A 2.1.0 needs a directory",
		}},
		modPackageFixture{fullName: "Ns-A", version: "2.1.0", files: map[string]string{
			"manifest.json": "{}", "plugins/A.dll": "a v2.1", "plugins/Shared/inside.dll": "inside",
		}},
		modPackageFixture{
			fullName: "Ns-Pack", version: "2.1.0", deps: []string{"Ns-D-1.0.0", "Ns-A-2.1.0"},
			files: map[string]string{"manifest.json": "{}"},
		},
	)
	seed(t, db, `UPDATE mod_packages SET categories = '["Modpacks"]' WHERE full_name = 'Ns-Pack'`)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	beforeTree := serverTree(t, dataDir)
	beforeRows := installedRows(t, db)

	preview := resolvePreview(t, rt, admin, "Ns-Pack", "2.1.0")
	if len(preview.Removals) != 1 || preview.Removals[0].FullName != "Ns-C" {
		t.Fatalf("removals = %+v, want Ns-C", preview.Removals)
	}

	got := installFails(t, rt, admin, "Ns-Pack", "2.1.0", "internal")
	if got.Progress < 85 {
		t.Fatalf("job failed at progress %d; it never reached the move phase (%+v)", got.Progress, got)
	}
	if tree := serverTree(t, dataDir); tree != beforeTree {
		t.Errorf("server/ after the rollback:\n%s\nwant:\n%s", tree, beforeTree)
	}
	if rows := installedRows(t, db); !reflect.DeepEqual(rows, beforeRows) {
		t.Errorf("rows after the rollback = %+v, want %+v", rows, beforeRows)
	}
	if n := preUpdateArchives(t, db); n != 1 {
		t.Errorf("pre_update archives = %d, want the backup kept after the failure", n)
	}
}

// TestTheSweepPutsBackAPackageAnInterruptedUpdateRemoved asserts the crash sweep restores a
// package a pack update had removed, both its files and its row.
func TestTheSweepPutsBackAPackageAnInterruptedUpdateRemoved(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	writeServerFile(t, dataDir, "BepInEx/plugins/C.dll", "c v1")
	writeServerFile(t, dataDir, "BepInEx/config/C.cfg", "the admin's settings")
	before := serverTree(t, dataDir)

	manifest, err := json.Marshal([]installer.ManifestEntry{
		{Path: "BepInEx/plugins/C.dll"}, {Path: "BepInEx/config/C.cfg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := store.InstanceMod{
		Source: source.Thunderstore, InstanceID: "inst-a", FullName: "Ns-C", Version: "1.0.0",
		InstalledAs: store.InstalledDependency, Side: store.SideUnknown, Enabled: true,
		FileManifest: string(manifest), InstalledAt: "2026-09-01T00:00:00Z",
	}

	// What the killed job had done: staged the removal, saved the file, recorded the row it was
	// replacing, emptied the row's manifest and removed the file.
	root := modStagingRoot(rt.instances.Cfg.Data.Root)
	staging, err := os.MkdirTemp(mkdirAllT(t, root), "install-*")
	if err != nil {
		t.Fatal(err)
	}
	mkdirAllT(t, filepath.Join(staging, "pkg", "Ns-C"))
	writeBackupFile(t, staging, "BepInEx/plugins/C.dll", "c v1")
	prev, err := json.Marshal(struct {
		Row   store.InstanceMod `json:"row"`
		Stale []string          `json:"stale"`
	}{Row: row, Stale: []string{"BepInEx/plugins/C.dll"}})
	if err != nil {
		t.Fatal(err)
	}
	mkdirAllT(t, filepath.Join(staging, "prev"))
	if err := os.WriteFile(filepath.Join(staging, "prev", "Ns-C.json"), prev, 0o644); err != nil {
		t.Fatal(err)
	}
	emptied := row
	emptied.FileManifest, emptied.InstalledAt = "[]", ""
	if err := db.WriteInstanceMods(t.Context(), "inst-a", []store.InstanceMod{emptied}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(serverPath(dataDir, "BepInEx/plugins/C.dll")); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(manager.InstallPayload{StagingDir: staging, FullName: "Ns-Pack", Version: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_install", manager.CheckpointManifestWritten, string(payload))

	if _, err := rt.supervisor.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after the sweep:\n%s\nwant:\n%s", got, before)
	}
	if got := installedRows(t, db)["Ns-C"]; got != row {
		t.Errorf("row after the sweep = %+v, want %+v", got, row)
	}
}

// TestTheSweepLeavesAnUpdateThatRecordedNothingAlone asserts a job stopped after staging an
// update and a removal, but before it backed anything up or rewrote a row, gets nothing rolled
// back: the installed files, config included, and the rows stay as they were.
func TestTheSweepLeavesAnUpdateThatRecordedNothingAlone(t *testing.T) {
	rt, db, _, _, dataDir := installWorld(t)
	writeServerFile(t, dataDir, "valheim_server.x86_64", "the game binary")
	writeServerFile(t, dataDir, "BepInEx/plugins/Only.dll", "v1")
	writeServerFile(t, dataDir, "BepInEx/config/Only.cfg", "the admin's settings")
	before := serverTree(t, dataDir)

	manifest, err := json.Marshal([]installer.ManifestEntry{
		{Path: "BepInEx/plugins/Only.dll"}, {Path: "BepInEx/config/Only.cfg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := store.InstanceMod{
		Source: source.Thunderstore, InstanceID: "inst-a", FullName: "Ns-Only", Version: "1.0.0",
		InstalledAs: store.InstalledExplicit, Side: store.SideUnknown, Enabled: true,
		FileManifest: string(manifest), InstalledAt: "2026-09-01T00:00:00Z",
	}
	if err := db.WriteInstanceMods(t.Context(), "inst-a", []store.InstanceMod{row}); err != nil {
		t.Fatal(err)
	}

	root := modStagingRoot(rt.instances.Cfg.Data.Root)
	staging, err := os.MkdirTemp(mkdirAllT(t, root), "install-*")
	if err != nil {
		t.Fatal(err)
	}
	staged := mkdirAllT(t, filepath.Join(staging, "pkg", "Ns-Only", "plugins"))
	if err := os.WriteFile(filepath.Join(staged, "Only.dll"), []byte("v2, staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(manager.InstallPayload{StagingDir: staging, FullName: "Ns-Only", Version: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, "mod_install", manager.CheckpointStaged, string(payload))

	if _, err := rt.supervisor.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := serverTree(t, dataDir); got != before {
		t.Errorf("server/ after the sweep:\n%s\nwant:\n%s", got, before)
	}
	if got := installedRows(t, db)["Ns-Only"]; got != row {
		t.Errorf("row after the sweep = %+v, want %+v", got, row)
	}
}

// TestADefinitionStepNeverLowersAPackage asserts a definition chain's install is a floor: a mod
// an earlier step raised past the version the definition pins stays where it is.
func TestADefinitionStepNeverLowersAPackage(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, twoVersions()...)
	alreadyModded(t, db)
	installOK(t, rt, admin, "Ns-Only", "2.0.0")

	pkgs, outcome := rt.mods.installer().ResolveForInstall(t.Context(), instanceRow(t, db), &manager.InstallPayload{
		FullName: "Ns-Only", Version: "1.0.0", Minimum: true,
	})
	if outcome != nil || len(pkgs) != 0 {
		t.Errorf("chain step = %d packages, outcome %+v; want nothing to do", len(pkgs), outcome)
	}
}

// TestARequestedRegistryIsNeverSwapped asserts the package an install names comes from the
// registry the request names: a version only another registry carries is unresolvable.
func TestARequestedRegistryIsNeverSwapped(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedBothRegistries(t, db)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/resolve",
		jsonBody(t, map[string]string{"full_name": "Only-Ts", "version": "2.0.0", "source": "hexium"})))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "dependency_unresolved" {
		t.Errorf("resolve = %d %s, want 409 dependency_unresolved", rec.Code, rec.Body)
	}
}
