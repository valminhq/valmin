package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// rollbackWorld is inst-a in the given state with the mod Ns-One 1.0.0 installed and the fake
// runtime scripted to save on stop.
func rollbackWorld(t *testing.T, state string) (rt *Server, db *store.DB, admin, member *store.User, dataDir string) {
	t.Helper()
	var fake *runtime.Fake
	rt, db, fake, admin, member = lifecycleWorld(t)
	seedInstance(t, rt, db, fake, state)
	savesOnStop(fake)
	dataDir = filepath.Join(rt.instances.Cfg.Data.HostRoot, "instances", "inst-a")
	setupTestMod(t, db, dataDir, "Ns-One", source.Thunderstore,
		"1.0.0", "one", false, true, false, "unknown")
	return rt, db, admin, member, dataDir
}

// rollback posts a rollback of setup id with body and an optional If-Match.
func rollback(
	t *testing.T, rt *Server, u *store.User, id, etag string, body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, setupsURL+"/"+id+"/rollback", jsonBody(t, body))
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	return as(rt, u, req)
}

// rollbackJob posts a rollback expected to be accepted and waits for its job.
func rollbackJob(t *testing.T, rt *Server, u *store.User, id string, body map[string]any) jobView {
	t.Helper()
	rec := rollback(t, rt, u, id, "", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rollback = %d %s, want 202", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	return waitJob(t, rt, u, accepted.JobID)
}

// instAState reads inst-a's state from the database.
func instAState(t *testing.T, db *store.DB) string {
	t.Helper()
	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// removeSetupMod uninstalls Ns-One behind the setups' back, so a restore has something to put
// back.
func removeSetupMod(t *testing.T, db *store.DB, dataDir string) {
	t.Helper()
	seed(t, db, `DELETE FROM instance_mods WHERE instance_id = 'inst-a'`)
	if err := os.Remove(serverPath(dataDir, "BepInEx/plugins/Ns-One.dll")); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackRestoresTheSetupAndItsWorld asserts a rollback of a stopped server restores the
// setup, restores the linked world when asked and archives the world it replaces, and leaves the
// server stopped.
func TestRollbackRestoresTheSetupAndItsWorld(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "stopped")
	seedWorldOnDisk(t, db)
	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, setupsURL,
		jsonBody(t, map[string]any{"name": "known good", "back_up_world": true})))
	var accepted jobView
	decodeInto(t, rec, &accepted)
	if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "succeeded" {
		t.Fatalf("save job = %+v, want succeeded", job)
	}
	rows, err := db.ListSetups(t.Context(), "inst-a")
	if err != nil || len(rows) != 1 || rows[0].BackupID == "" {
		t.Fatalf("setups = %+v, %v, want one linked to a world backup", rows, err)
	}
	removeSetupMod(t, db, dataDir)

	job := rollbackJob(t, rt, admin, rows[0].ID, map[string]any{"world": true})

	if job.Status != "succeeded" {
		t.Fatalf("rollback job = %+v, want succeeded", job)
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-One.dll"), "one")
	if got := instAState(t, db); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}
	var restores int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM backups
		WHERE instance_id = 'inst-a' AND trigger = ?`, store.TriggerPreRestore).Scan(&restores); err != nil || restores != 1 {
		t.Errorf("pre_restore snapshots = %d, %v, want the world restore's one", restores, err)
	}
}

// TestRollbackRestoresAChosenWorldBackup asserts a setup's world backups are the backups taken
// under its mods, that a rollback restores the one it names, and that it refuses one taken under
// other mods.
func TestRollbackRestoresAChosenWorldBackup(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "stopped")
	seedWorldOnDisk(t, db)
	marker := filepath.Join(worldsDirOfID(t, db, "inst-a"), "worlds_local", "marker.txt")
	taken := make([]string, 0, 2)
	for _, content := range []string{"older", "newer"} {
		if err := os.WriteFile(marker, []byte(content), 0o664); err != nil {
			t.Fatal(err)
		}
		if job := waitJob(t, rt, admin, postBackup(t, rt, admin, "").JobID); job.Status != "succeeded" {
			t.Fatalf("backup job = %+v", job)
		}
		taken = append(taken, newestBackupID(t, db))
	}
	if err := db.CreateBackup(t.Context(), &store.Backup{
		ID: "other-mods", InstanceID: "inst-a", Path: marker, SHA256: "hash", WorldName: "World",
		Trigger: store.TriggerManual, Consistent: true, ModsFingerprint: "other",
	}); err != nil {
		t.Fatal(err)
	}
	saved := setupTestSave(t, rt, db, admin, "no world linked")
	if saved.WorldBackups != 2 {
		t.Errorf("listed world backups = %d, want 2", saved.WorldBackups)
	}
	preview := setupTestPreview(t, rt, admin, saved.ID)
	if len(preview.Setup.WorldBackups) != 2 || preview.Setup.WorldBackups[0].ID != taken[1] ||
		preview.Setup.WorldBackups[1].ID != taken[0] || !preview.Setup.WorldBackups[1].Available {
		t.Fatalf("world backups = %+v, want %v newest first and available", preview.Setup.WorldBackups, taken)
	}
	rec := rollback(t, rt, admin, saved.ID, "", map[string]any{"world_backup_id": "other-mods"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("rollback to a backup of other mods = %d %s, want 422", rec.Code, rec.Body)
	}
	removeSetupMod(t, db, dataDir)

	job := rollbackJob(t, rt, admin, saved.ID, map[string]any{"world_backup_id": taken[0]})

	if job.Status != "succeeded" {
		t.Fatalf("rollback job = %+v, want succeeded", job)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "older" {
		t.Errorf("world marker = %q, %v, want the older backup's", got, err)
	}
	var replaced string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT mods_fingerprint FROM backups
		WHERE instance_id = 'inst-a' AND trigger = ?`, store.TriggerPreRestore).Scan(&replaced); err != nil {
		t.Fatal(err)
	}
	if replaced != control.ModsFingerprint(nil) {
		t.Errorf("the replaced world records mods %q, want those it was played with, none (%q)",
			replaced, control.ModsFingerprint(nil))
	}
	if got, err := db.WorldModsFingerprint(t.Context(), "inst-a"); err != nil || got != saved.ModsFingerprint {
		t.Errorf(
			"world mods after the rollback = %q, %v, want the restored backup's %q",
			got,
			err,
			saved.ModsFingerprint,
		)
	}
}

// TestABackupRecordsTheModsTheWorldWasPlayedWith asserts a backup taken after a mod change made
// while the server was stopped records the mods the server last ran with, not the installed ones.
func TestABackupRecordsTheModsTheWorldWasPlayedWith(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "stopped")
	seedWorldOnDisk(t, db)
	played, err := rt.instances.ctl.SetupState.ModsFingerprint(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "stop"} {
		rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/"+action, http.NoBody))
		var accepted jobView
		decodeInto(t, rec, &accepted)
		if job := waitJob(t, rt, admin, accepted.JobID); job.Status != "succeeded" {
			t.Fatalf("%s job = %+v, want succeeded", action, job)
		}
	}
	removeSetupMod(t, db, dataDir)

	if job := waitJob(t, rt, admin, postBackup(t, rt, admin, "").JobID); job.Status != "succeeded" {
		t.Fatalf("backup job = %+v", job)
	}

	b, err := db.BackupByID(t.Context(), "inst-a", newestBackupID(t, db))
	if err != nil || b == nil {
		t.Fatalf("BackupByID = %+v, %v", b, err)
	}
	if b.ModsFingerprint != played || played == control.ModsFingerprint(nil) {
		t.Errorf("backup records mods %q, want those the server ran with, %q", b.ModsFingerprint, played)
	}
}

// newestBackupID is the ID of inst-a's newest backup.
func newestBackupID(t *testing.T, db *store.DB) string {
	t.Helper()
	var id string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT id FROM backups WHERE instance_id = 'inst-a'
		ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRollbackStopsARunningServerAndStartsItAgain asserts a rollback of a running server stops
// it, restores the setup and starts it again when asked.
func TestRollbackStopsARunningServerAndStartsItAgain(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "running")
	saved := setupTestSave(t, rt, db, admin, "while running")
	removeSetupMod(t, db, dataDir)

	job := rollbackJob(t, rt, admin, saved.ID, map[string]any{"start": true})

	if job.Status != "succeeded" {
		t.Fatalf("rollback job = %+v, want succeeded", job)
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-One.dll"), "one")
	if got := instAState(t, db); got != "running" {
		t.Errorf("state = %q, want running", got)
	}
	if got, err := db.WorldModsFingerprint(t.Context(), "inst-a"); err != nil || got != saved.ModsFingerprint {
		t.Errorf("world mods after the start = %q, %v, want the restored setup's %q", got, err, saved.ModsFingerprint)
	}
	var stops int
	err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE instance_id = 'inst-a' AND kind = 'stop'`).Scan(&stops)
	if err != nil || stops != 1 {
		t.Errorf("stop jobs = %d, %v, want one", stops, err)
	}
}

// TestRollbackStopsAtAFailedSetupRestore asserts a failed setup restore starts no server, and that
// the server stopped for the rollback stays stopped.
func TestRollbackStopsAtAFailedSetupRestore(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "running")
	saved := setupTestSave(t, rt, db, admin, "while running")
	removeSetupMod(t, db, dataDir)
	rt.instances.ctl.SetupJobs.Apply = func(*store.Instance, string, control.SetupFiles, control.SetupFiles) error {
		return errors.New("injected restore failure")
	}

	job := rollbackJob(t, rt, admin, saved.ID, map[string]any{"start": true})

	if job.Status != "failed" {
		t.Fatalf("rollback job = %+v, want failed", job)
	}
	if got := instAState(t, db); got != "stopped" {
		t.Errorf("state = %q, want stopped: the start must not run after a failure", got)
	}
}

// TestRollbackChecksTheRequest asserts a rollback needs the right to manage setups and the rights
// its steps need, refuses a world without an available linked backup, a stale fingerprint and a
// server between states, and names nothing it cannot find.
func TestRollbackChecksTheRequest(t *testing.T) {
	t.Parallel()
	rt, db, admin, member, _ := rollbackWorld(t, "stopped")
	saved := setupTestSave(t, rt, db, admin, "plain")

	if rec := rollback(t, rt, member, saved.ID, "", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Errorf("member rollback = %d %s, want 403", rec.Code, rec.Body)
	}
	rec := rollback(t, rt, admin, saved.ID, "", map[string]any{"world": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("world without a linked backup = %d %s, want 422", rec.Code, rec.Body)
	}
	rec = rollback(t, rt, admin, saved.ID, `"stale"`, map[string]any{})
	if rec.Code != http.StatusPreconditionFailed || errCode(t, rec) != "stale_write" {
		t.Errorf("stale fingerprint = %d %s, want 412 stale_write", rec.Code, rec.Body)
	}
	if rec := rollback(t, rt, admin, "missing", "", map[string]any{}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown setup = %d %s, want 404", rec.Code, rec.Body)
	}
	seed(t, db, `UPDATE instances SET state = 'starting' WHERE id = 'inst-a'`)
	rec = rollback(t, rt, admin, saved.ID, "", map[string]any{})
	if rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Errorf("rollback while starting = %d %s, want 409 invalid_state", rec.Code, rec.Body)
	}
}

// TestPreviewSaysWhenTheServerMustBeStopped asserts a running server's preview is ready with
// stop_required set, and a stopped one's is not.
func TestPreviewSaysWhenTheServerMustBeStopped(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, _ := rollbackWorld(t, "running")
	saved := setupTestSave(t, rt, db, admin, "while running")
	if p := setupTestPreview(t, rt, admin, saved.ID); !p.StopRequired || !p.Ready {
		t.Errorf("running preview: stop_required = %v, ready = %v, want true and true (%v)",
			p.StopRequired, p.Ready, p.Problems)
	}
	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	if p := setupTestPreview(t, rt, admin, saved.ID); p.StopRequired {
		t.Errorf("stopped preview: stop_required = true, want false")
	}
}

// TestRollbackWaitsForAJobHoldingTheSetupLock asserts a rollback whose setup restore meets a job
// holding a lock it needs waits for that job and then restores the setup.
func TestRollbackWaitsForAJobHoldingTheSetupLock(t *testing.T) {
	t.Parallel()
	rt, db, admin, _, dataDir := rollbackWorld(t, "stopped")
	saved := setupTestSave(t, rt, db, admin, "plain")
	removeSetupMod(t, db, dataDir)
	release := make(chan struct{})
	holder, err := rt.instances.Engine.Submit(t.Context(), &jobs.Spec{
		Kind: jobs.KindPrune, LockKey: control.SetupArtifactLock,
	}, func(context.Context, *jobs.Handle) jobs.Outcome {
		<-release
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := rollback(t, rt, admin, saved.ID, "", map[string]any{})
	if rec.Code != http.StatusAccepted {
		close(release)
		t.Fatalf("rollback = %d %s, want 202", rec.Code, rec.Body)
	}
	var accepted jobView
	decodeInto(t, rec, &accepted)
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	job := waitJob(t, rt, admin, accepted.JobID)

	if job.Status != "succeeded" {
		t.Fatalf("rollback job = %+v, want succeeded once %s released the lock", job, holder.ID)
	}
	if log := jobLog(t, db, accepted.JobID); !strings.Contains(log, "waiting for a prune job to finish") {
		t.Errorf("rollback log = %q, want it to say it waited for the prune job", log)
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-One.dll"), "one")
}

// grantMember gives the seeded member the operator role on inst-a with perms added.
func grantMember(t *testing.T, db *store.DB, perms ...string) {
	t.Helper()
	raw, err := json.Marshal(perms)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instance_grants SET role = 'operator', perms = ?
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`, string(raw))
}

// TestAMemberWhoManagesModsRollsBackModsOnly asserts a member who may manage mods sees the
// setups and rolls back mods and config files while the server's settings stay as they are, even
// without asking for that, and reads no config text without config.raw, while saving and
// deleting stay out of reach.
func TestAMemberWhoManagesModsRollsBackModsOnly(t *testing.T) {
	t.Parallel()
	rt, db, admin, member, dataDir := rollbackWorld(t, "stopped")
	writeServerFile(t, dataDir, "BepInEx/config/one.cfg", "a = 1\n")
	saved := setupTestSave(t, rt, db, admin, "known good")
	removeSetupMod(t, db, dataDir)
	seed(t, db, `UPDATE instances SET server_name = 'Renamed', mem_limit_mb = 6144 WHERE id = 'inst-a'`)
	grantMember(t, db, "mods.manage")

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, setupsURL, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("member list = %d %s, want 200", rec.Code, rec.Body)
	}
	preview := setupTestPreview(t, rt, member, saved.ID)
	for _, c := range append(preview.Current.Configs, preview.Setup.Configs...) {
		if c.Content != nil {
			t.Errorf("config %s carries its text for a member without config.raw", c.File)
		}
	}
	text := as(rt, member, httptest.NewRequest(http.MethodGet,
		setupsURL+"/"+saved.ID+"/configs/one.cfg", http.NoBody))
	if text.Code != http.StatusForbidden {
		t.Errorf("member config text = %d, want 403", text.Code)
	}
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"save", httptest.NewRequest(http.MethodPost, setupsURL, jsonBody(t, map[string]string{"name": "x"}))},
		{"delete", httptest.NewRequest(http.MethodDelete, setupsURL+"/"+saved.ID, http.NoBody)},
		{"restore", httptest.NewRequest(http.MethodPost, setupsURL+"/"+saved.ID+"/restore", http.NoBody)},
	} {
		if rec := as(rt, member, tc.req); rec.Code != http.StatusForbidden {
			t.Errorf("member %s = %d, want 403", tc.name, rec.Code)
		}
	}
	job := rollbackJob(t, rt, member, saved.ID, map[string]any{})

	if job.Status != "succeeded" {
		t.Fatalf("member rollback = %+v, want succeeded", job)
	}
	setupTestFile(t, serverPath(dataDir, "BepInEx/plugins/Ns-One.dll"), "one")
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.ServerName != "Renamed" || inst.MemLimitMB != 6144 {
		t.Errorf("settings after a mods-only rollback = %q, %d MB, want Renamed and 6144 kept",
			inst.ServerName, inst.MemLimitMB)
	}
}

// TestAViewerCannotSeeSetups asserts a member without mods.manage cannot list setups or roll
// back.
func TestAViewerCannotSeeSetups(t *testing.T) {
	t.Parallel()
	rt, db, admin, member, _ := rollbackWorld(t, "stopped")
	saved := setupTestSave(t, rt, db, admin, "plain")
	grantMember(t, db, "config.raw")
	if rec := as(
		rt,
		member,
		httptest.NewRequest(http.MethodGet, setupsURL, http.NoBody),
	); rec.Code != http.StatusForbidden {
		t.Errorf("list = %d, want 403", rec.Code)
	}
	if rec := rollback(
		t,
		rt,
		member,
		saved.ID,
		"",
		map[string]any{"mods_only": true},
	); rec.Code != http.StatusForbidden {
		t.Errorf("rollback = %d, want 403", rec.Code)
	}
}
