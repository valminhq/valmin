package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

const setupsURL = "/api/v1/instances/inst-a/setups"

func setupTestWorld(t *testing.T) (rt *Server, db *store.DB, admin, member *store.User, dataDir string) {
	t.Helper()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	return rt, db, admin, member, filepath.Join(rt.instances.Cfg.Data.HostRoot, "instances", "inst-a")
}

func setupTestMod(t *testing.T, db *store.DB, dataDir, name string, registry source.Source,
	version, body string, parked, enabled, locked bool, side string,
) {
	t.Helper()
	path := "BepInEx/plugins/" + name + ".dll"
	root := filepath.Join(dataDir, "server")
	if parked {
		root = filepath.Join(instance.ParkedModsDir(dataDir), name)
	}
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	raw, err := json.Marshal([]installer.ManifestEntry{{
		Path: path, SHA256: hex.EncodeToString(sum[:]), Parked: parked,
	}})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO instance_mods (
		instance_id, full_name, source, version, installed_as, side,
		enabled, locked, file_manifest, installed_at
	) VALUES ('inst-a', ?, ?, ?, 'explicit', ?, ?, ?, ?, ?)`,
		name, registry.String(), version, side, enabled, locked, string(raw), store.Now())
}

func setupTestSave(t *testing.T, rt *Server, db *store.DB, admin *store.User, name string) store.SavedSetup {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, setupsURL,
		jsonBody(t, map[string]string{"name": name})))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("save = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "succeeded" {
		t.Fatalf("save job = %+v, want succeeded", job)
	}
	rows, err := db.ListSetups(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("save succeeded without publishing setup")
	}
	return rows[0]
}

func setupTestPreview(t *testing.T, rt *Server, admin *store.User, id string) setupPreviewView {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, setupsURL+"/"+id+"/preview", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var preview setupPreviewView
	decodeInto(t, rec, &preview)
	return preview
}

func setupTestRestore(t *testing.T, rt *Server, admin *store.User, id, etag string) jobView {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, setupsURL+"/"+id+"/restore", http.NoBody)
	req.Header.Set("If-Match", etag)
	rec := as(rt, admin, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	return waitJob(t, rt, admin, accepted.JobID)
}

func setupTestFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func TestSetupRestoresOfflineModsParkedFilesConfigurationAndSettings(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-Same", source.Thunderstore,
		"1.0.0", "thunderstore bytes", false, true, true, "client_required")
	setupTestMod(t, db, dataDir, "Ns-Off", source.Hexium,
		"2.0.0", "parked bytes", true, false, false, "client_optional")
	writeServerFile(t, dataDir, "BepInEx/config/Mod.cfg", "saved config")
	writeServerFile(t, dataDir, "BepInEx/config/"+command.ConfigFile, "original secret")
	writeServerFile(t, dataDir, "BepInEx/plugins/Operator.dll", "unmanaged")
	seed(t, db, `UPDATE instances SET server_name = 'Saved Server', public = TRUE,
		crossplay = TRUE, mem_limit_mb = 8192, backup_keep_cold = 7,
		backup_keep_hot = 4, backup_on_restart = TRUE, game_build_id = 'saved-build'
		WHERE id = 'inst-a'`)
	saved := setupTestSave(t, rt, db, admin, "Working before update")
	if strings.Contains(saved.SnapshotJSON, command.ConfigFile) {
		t.Fatal("setup included generated RCON credential configuration")
	}
	_, refs, err := db.SetupByID(t.Context(), "inst-a", saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("artifact references = %d, want 2", len(refs))
	}
	for _, ref := range refs {
		if _, err := setupblob.New(rt.instances.Cfg.Data.Root).Verify(ref.SHA256); err != nil {
			t.Fatalf("retained %s: %v", ref.FullName, err)
		}
	}
	// No registry rows or cache are available during restore.
	if err := os.RemoveAll(filepath.Join(rt.instances.Cfg.Data.Root, "cache")); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `DELETE FROM instance_mods WHERE instance_id = 'inst-a'`)
	if err := os.RemoveAll(filepath.Join(dataDir, "server", "BepInEx", "plugins", "Ns-Same.dll")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(instance.ParkedModsDir(dataDir), "Ns-Off")); err != nil {
		t.Fatal(err)
	}
	setupTestMod(t, db, dataDir, "Ns-Same", source.Hexium,
		"9.0.0", "hexium bytes", false, true, false, "server_only")
	setupTestMod(t, db, dataDir, "Ns-New", source.Hexium,
		"1.0.0", "new bytes", false, true, false, "unknown")
	writeServerFile(t, dataDir, "BepInEx/config/Mod.cfg", "changed config")
	writeServerFile(t, dataDir, "BepInEx/config/"+command.ConfigFile, "current secret")
	writeServerFile(t, dataDir, "BepInEx/config/New.cfg", "remove this config")
	seed(t, db, `UPDATE instances SET server_name = 'Changed Server', public = FALSE,
		crossplay = FALSE, mem_limit_mb = 4096, backup_keep_cold = 1,
		backup_keep_hot = 1, backup_on_restart = FALSE, world_name = 'CurrentWorld',
		game_build_id = 'current-build' WHERE id = 'inst-a'`)

	preview := setupTestPreview(t, rt, admin, saved.ID)
	if !preview.Ready || preview.ETag == "" {
		t.Fatalf("preview = %+v, want ready with fingerprint", preview)
	}
	if preview.Setup.GameBuildID != "saved-build" || preview.Setup.WorldName != "World" {
		t.Errorf("saved comparison fields = %+v", preview.Setup)
	}
	if preview.Current.Build != "current-build" || preview.Current.Launch.WorldName != "CurrentWorld" {
		t.Errorf("current comparison fields = %+v", preview.Current)
	}
	if job := setupTestRestore(t, rt, admin, saved.ID, preview.ETag); job.Status != "succeeded" {
		t.Fatalf("restore job = %+v, want succeeded", job)
	}
	rows := installedRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("restored mods = %+v, want 2", rows)
	}
	same := rows["Ns-Same"]
	if same.Source != source.Thunderstore || same.Version != "1.0.0" ||
		!same.Enabled || !same.Locked || same.Side != "client_required" {
		t.Errorf("same-name package = %+v, want saved Thunderstore package", same)
	}
	off := rows["Ns-Off"]
	if off.Source != source.Hexium || off.Version != "2.0.0" ||
		off.Enabled || off.Locked || off.Side != "client_optional" {
		t.Errorf("disabled package = %+v, want saved parked package", off)
	}
	if _, exists := rows["Ns-New"]; exists {
		t.Error("restore retained package added after save")
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-Same.dll"), "thunderstore bytes")
	setupTestFile(t, filepath.Join(instance.ParkedModsDir(dataDir), "Ns-Off",
		"BepInEx", "plugins", "Ns-Off.dll"), "parked bytes")
	setupTestFile(t, serverPath(dataDir, "BepInEx/config/Mod.cfg"), "saved config")
	setupTestFile(t, serverPath(dataDir, "BepInEx/config/"+command.ConfigFile), "current secret")
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Operator.dll"), "unmanaged")
	for _, path := range []string{"BepInEx/config/New.cfg", "BepInEx/plugins/Ns-New.dll"} {
		if _, err := os.Stat(serverPath(dataDir, path)); !os.IsNotExist(err) {
			t.Errorf("%s after restore: %v, want absent", path, err)
		}
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "stopped" || inst.ServerName != "Saved Server" || !inst.Public ||
		!inst.Crossplay || inst.MemLimitMB != 8192 || inst.BackupKeepCold != 7 ||
		inst.BackupKeepHot != 4 || !inst.BackupOnRestart {
		t.Errorf("restored settings = %+v", inst)
	}
	if inst.WorldName != "CurrentWorld" || inst.GameBuildID == nil || *inst.GameBuildID != "current-build" {
		t.Errorf("comparison-only fields changed: world=%q build=%v", inst.WorldName, inst.GameBuildID)
	}
}

func TestSetupPreviewReportsUnmanagedFileConflict(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "saved bytes", false, true, false, "unknown")
	saved := setupTestSave(t, rt, db, admin, "before")
	seed(t, db, `DELETE FROM instance_mods WHERE instance_id = 'inst-a'`)
	writeServerFile(t, dataDir, "BepInEx/plugins/Ns-One.dll", "unmanaged bytes")

	preview := setupTestPreview(t, rt, admin, saved.ID)
	if preview.Ready || !strings.Contains(strings.Join(preview.Problems, " "), "unmanaged file") {
		t.Fatalf("preview = %+v, want an unmanaged file conflict", preview)
	}
}

func TestSetupRestoreRequiresCurrentPreview(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "one", false, true, false, "unknown")
	saved := setupTestSave(t, rt, db, admin, "before")
	preview := setupTestPreview(t, rt, admin, saved.ID)
	writeServerFile(t, dataDir, "BepInEx/config/Change.cfg", "changed after preview")
	req := httptest.NewRequest(http.MethodPost, setupsURL+"/"+saved.ID+"/restore", http.NoBody)
	req.Header.Set("If-Match", preview.ETag)
	rec := as(rt, admin, req)
	if rec.Code != http.StatusPreconditionFailed || errCode(t, rec) != "stale_write" {
		t.Fatalf("stale restore = %d %s, want 412 stale_write", rec.Code, rec.Body.String())
	}
}

func TestSetupPreviewRejectsMissingOrCorruptArtifact(t *testing.T) {
	for _, change := range []string{"remove", "corrupt"} {
		t.Run(change, func(t *testing.T) {
			rt, db, admin, _, dataDir := setupTestWorld(t)
			setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
				"1.0.0", "one", false, true, false, "unknown")
			saved := setupTestSave(t, rt, db, admin, "before")
			_, refs, err := db.SetupByID(t.Context(), "inst-a", saved.ID)
			if err != nil || len(refs) != 1 {
				t.Fatalf("setup refs = %+v: %v", refs, err)
			}
			path, err := setupblob.New(rt.instances.Cfg.Data.Root).Verify(refs[0].SHA256)
			if err != nil {
				t.Fatal(err)
			}
			if change == "remove" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("corrupt"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			preview := setupTestPreview(t, rt, admin, saved.ID)
			if preview.Ready || len(preview.Problems) == 0 {
				t.Fatalf("%s artifact preview = %+v, want blocked", change, preview)
			}
			req := httptest.NewRequest(http.MethodPost, setupsURL+"/"+saved.ID+"/restore", http.NoBody)
			req.Header.Set("If-Match", preview.ETag)
			rec := as(rt, admin, req)
			if rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
				t.Fatalf("%s artifact restore = %d %s, want 409 invalid_state", change, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSetupActionsRequireAdministratorAndStoppedServer(t *testing.T) {
	rt, db, admin, member, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "one", false, true, false, "unknown")
	saved := setupTestSave(t, rt, db, admin, "before")
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, setupsURL, nil},
		{http.MethodPost, setupsURL, map[string]string{"name": "new"}},
		{http.MethodGet, setupsURL + "/" + saved.ID, nil},
		{http.MethodGet, setupsURL + "/" + saved.ID + "/preview", nil},
		{http.MethodPost, setupsURL + "/" + saved.ID + "/restore", nil},
		{http.MethodDelete, setupsURL + "/" + saved.ID, nil},
	} {
		var req *http.Request
		if tc.body == nil {
			req = httptest.NewRequest(tc.method, tc.path, http.NoBody)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, jsonBody(t, tc.body))
		}
		rec := as(rt, member, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s by member = %d %s, want 403", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, setupsURL,
		jsonBody(t, map[string]string{"name": "running"})))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "instance_must_be_stopped" {
		t.Errorf("save while running = %d %s", rec.Code, rec.Body.String())
	}
	rec = as(rt, admin, httptest.NewRequest(http.MethodPost,
		setupsURL+"/"+saved.ID+"/restore", http.NoBody))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "instance_must_be_stopped" {
		t.Errorf("restore while running = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSetupSaveDoesNotPublishMissingInstalledFiles(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "one", false, true, false, "unknown")
	if err := os.Remove(serverPath(dataDir, "BepInEx/plugins/Ns-One.dll")); err != nil {
		t.Fatal(err)
	}
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, setupsURL,
		jsonBody(t, map[string]string{"name": "broken"})))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("save = %d %s, want queued job", rec.Code, rec.Body.String())
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "failed" {
		t.Fatalf("missing-file save = %+v, want failed", job)
	}
	rows, err := db.ListSetups(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("failed save published %d setups", len(rows))
	}
}

func TestInterruptedSetupRestoreRecoversOriginalFiles(t *testing.T) {
	rt, db, _, _, dataDir := setupTestWorld(t)
	original := "before interrupted restore"
	writeServerFile(t, dataDir, "BepInEx/plugins/Ns-One.dll", original)
	staging, err := os.MkdirTemp(mkdirAllT(t, control.SetupStagingRoot(rt.instances.Cfg.Data.Root)), "job-")
	if err != nil {
		t.Fatal(err)
	}
	journal := control.SetupJournal{Roots: []control.SetupRootPaths{{
		Root: "server", Paths: []string{"BepInEx/plugins/Ns-One.dll", "BepInEx/plugins/Ns-New.dll"},
	}}}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := control.BackupSetupPaths(inst, staging, journal); err != nil {
		t.Fatal(err)
	}
	if err := control.WriteSetupJournal(staging, journal); err != nil {
		t.Fatal(err)
	}
	writeServerFile(t, dataDir, "BepInEx/plugins/Ns-One.dll", "partial restore")
	writeServerFile(t, dataDir, "BepInEx/plugins/Ns-New.dll", "partial new file")
	raw, err := json.Marshal(control.SetupJobPayload{SetupID: "setup-old", StagingDir: staging})
	if err != nil {
		t.Fatal(err)
	}
	jobID := seedStaleJob(t, db, "setup_restore", "applied", string(raw))
	if _, err := rt.supervisor.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-One.dll"), original)
	if _, err := os.Stat(serverPath(dataDir, "BepInEx/plugins/Ns-New.dll")); !os.IsNotExist(err) {
		t.Errorf("new file after recovery: %v, want absent", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging after recovery: %v, want absent", err)
	}
	if got := jobRow(t, db, jobID); got.Status != "failed" || got.ErrorCode == nil ||
		*got.ErrorCode != "interrupted" {
		t.Errorf("interrupted restore = %+v", got)
	}
}

// A payload must be present even if its archive was already cached when the setup was saved.
func TestSetupSavePinsAReproducibleCachedArchive(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "one", false, true, false, "unknown")
	cacheDir := filepath.Join(rt.instances.Cfg.Data.Root, "cache", "thunderstore")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(cacheDir, "Ns-One-1.0.0.zip")
	if err := os.WriteFile(archive, modZip(t, map[string]string{"plugins/Ns-One.dll": "one"}), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := setupTestSave(t, rt, db, admin, "cached")
	_, refs, err := db.SetupByID(t.Context(), "inst-a", saved.ID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %+v: %v", refs, err)
	}
	if refs[0].Kind != "zip" {
		t.Errorf("artifact kind = %q, want zip", refs[0].Kind)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	preview := setupTestPreview(t, rt, admin, saved.ID)
	if !preview.Ready || strings.Contains(strings.Join(preview.Problems, " "), "unavailable") {
		t.Errorf("preview after cache deletion = %+v, want ready", preview)
	}
}

func TestSetupRestoreRollsBackAfterFileWriteFailure(t *testing.T) {
	rt, db, admin, _, dataDir := setupTestWorld(t)
	setupTestMod(t, db, dataDir, "Ns-Saved", source.Thunderstore,
		"1.0.0", "saved bytes", false, true, true, "client_required")
	seed(t, db, `UPDATE instances SET server_name = 'Saved Server' WHERE id = 'inst-a'`)
	saved := setupTestSave(t, rt, db, admin, "before")
	seed(t, db, `DELETE FROM instance_mods WHERE instance_id = 'inst-a'`)
	if err := os.Remove(serverPath(dataDir, "BepInEx/plugins/Ns-Saved.dll")); err != nil {
		t.Fatal(err)
	}
	setupTestMod(t, db, dataDir, "Ns-Current", source.Hexium,
		"2.0.0", "current bytes", false, true, false, "server_only")
	seed(t, db, `UPDATE instances SET server_name = 'Current Server' WHERE id = 'inst-a'`)
	before := serverTree(t, dataDir)
	preview := setupTestPreview(t, rt, admin, saved.ID)
	if !preview.Ready {
		t.Fatalf("preview = %+v, want ready", preview)
	}
	rt.instances.setupApply = func(inst *store.Instance, staging string,
		current, _ map[string]map[string]bool,
	) error {
		first := map[string]map[string]bool{
			"server": {"BepInEx/plugins/Ns-Saved.dll": true},
		}
		if err := control.ApplySetupPaths(inst, staging, current, first); err != nil {
			return err
		}
		return errors.New("injected file-write failure after first placement")
	}
	job := setupTestRestore(t, rt, admin, saved.ID, preview.ETag)
	if job.Status != "failed" || job.Progress < 80 {
		t.Fatalf("restore = %+v, want failure after file placement", job)
	}
	if after := serverTree(t, dataDir); after != before {
		t.Errorf("server after failed restore:\n%s\nwant:\n%s", after, before)
	}
	rows := installedRows(t, db)
	if len(rows) != 1 || rows["Ns-Current"].Source != source.Hexium {
		t.Errorf("mods after failed restore = %+v, want original row", rows)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "stopped" || inst.ServerName != "Current Server" {
		t.Errorf("settings after failed restore = %+v, want original stopped settings", inst)
	}
}
