package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

func restorePath(backupID string) string { return backupsPath + "/" + backupID + "/restore" }

// worldFiles reads the live world back as a map, so a restore can be compared byte for byte
// against what the archive held.
func worldFiles(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	dir := filepath.Join(worldsDirOf(t, db), "worlds_local")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// takeBackup runs a real backup job and returns the catalogue id of the archive it wrote, so
// a restore test starts from an archive the panel itself produced.
func takeBackup(t *testing.T, rt *Router, admin *store.User) string {
	t.Helper()
	stub := postBackup(t, rt, admin, "")
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("backup job = %+v", final)
	}
	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 1 {
		t.Fatalf("catalogue has %d archives, want 1", len(page.Items))
	}
	id, _ := page.Items[0]["id"].(string)
	return id
}

func postRestore(t *testing.T, rt *Router, u *store.User, backupID string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, restorePath(backupID), http.NoBody))
}

// runRestoreJob submits a restore and waits for it, returning the finished job.
func runRestoreJob(t *testing.T, rt *Router, u *store.User, backupID string) jobView {
	t.Helper()
	rec := postRestore(t, rt, u, backupID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	return waitJob(t, rt, u, stub.JobID)
}

// corruptTheWorld overwrites the live save with something no server could load, which is the
// situation a restore exists for.
func corruptTheWorld(t *testing.T, db *store.DB) {
	t.Helper()
	path := filepath.Join(worldsDirOf(t, db), "worlds_local", seededWorldName+".db")
	if err := os.WriteFile(path, []byte("corrupt"), 0o664); err != nil {
		t.Fatal(err)
	}
}

// backupsWithTrigger reports the catalogue ids carrying one trigger value.
func backupsWithTrigger(t *testing.T, rt *Router, admin *store.User, trigger string) []string {
	t.Helper()
	var out []string
	for _, item := range listBackupsAs(t, rt, admin, "").Items {
		if item["trigger"] == trigger {
			id, _ := item["id"].(string)
			out = append(out, id)
		}
	}
	return out
}

// Asserts a world deliberately corrupted comes back byte-identical to what the archive held,
// with no manual filesystem work anywhere in the path.
func TestRestoreBringsTheArchivedWorldBackByteForByte(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin

	backupID := takeBackup(t, rt, admin)
	before := worldFiles(t, db)
	corruptTheWorld(t, db)

	if final := runRestoreJob(t, rt, admin, backupID); final.Status != "succeeded" {
		t.Fatalf("restore job = %+v", final)
	}
	if got := worldFiles(t, db); !equalFiles(got, before) {
		t.Errorf("world after restore = %v, want the archived world", keysOf(got))
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}
}

func equalFiles(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for name, body := range want {
		if got[name] != body {
			return false
		}
	}
	return true
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Asserts the swap leaves neither staging directory behind: a stray worlds_local.new sits
// inside the savedir the container binds.
func TestRestoreLeavesNoStagingDirectoryBehind(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin

	backupID := takeBackup(t, rt, admin)
	if final := runRestoreJob(t, rt, admin, backupID); final.Status != "succeeded" {
		t.Fatalf("restore job = %+v", final)
	}

	for _, leftover := range []string{"worlds_local.new", "worlds_local.old"} {
		path := filepath.Join(worldsDirOf(t, db), leftover)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the restore", leftover)
		}
	}
}

// Asserts the mandatory pre-restore snapshot is in the catalogue after a restore that worked.
func TestRestoreSnapshotsTheWorldItReplaces(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, admin := w.rt, w.admin

	backupID := takeBackup(t, rt, admin)
	if final := runRestoreJob(t, rt, admin, backupID); final.Status != "succeeded" {
		t.Fatalf("restore job = %+v", final)
	}

	if snaps := backupsWithTrigger(t, rt, admin, store.TriggerPreRestore); len(snaps) != 1 {
		t.Errorf("catalogue holds %d pre_restore archives, want 1", len(snaps))
	}
}

// seedUnrestorableArchive writes an archive that verifies — the pair is in it, at a plausible
// size — but carries no world under worlds_local/, so it fails at staging rather than at the
// check. That is the failure that has to happen after the snapshot has been taken.
func seedUnrestorableArchive(t *testing.T, db *store.DB, root string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{
		"stray/" + seededWorldName + ".db":  dbBytes(),
		"stray/" + seededWorldName + ".fwl": fwlBytes(37, seededWorldName),
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o664, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "backups", "inst-a")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stray.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o664); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO backups
		(id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES ('b-stray', 'inst-a', ?, ?, 'abc123', ?, 'manual', 1, ?)`,
		path, buf.Len(), seededWorldName, store.Now())
	return "b-stray"
}

// Asserts a restore that fails after its snapshot parks the instance in error, leaves the
// world it was replacing untouched, and keeps the snapshot in the catalogue (B7).
func TestRestoreThatFailsParksInErrorWithItsSnapshotKept(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	root := rt.Supervisor().inst.Cfg.Data.Root

	before := worldFiles(t, db)
	backupID := seedUnrestorableArchive(t, db, root)

	final := runRestoreJob(t, rt, admin, backupID)
	if final.Status != "failed" {
		t.Fatalf("restore job = %+v, want failed", final)
	}
	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %q, want error (B7)", got)
	}
	if got := worldFiles(t, db); !equalFiles(got, before) {
		t.Error("a failed restore changed the world it was replacing")
	}
	if snaps := backupsWithTrigger(t, rt, admin, store.TriggerPreRestore); len(snaps) != 1 {
		t.Errorf("catalogue holds %d pre_restore archives after a failure, want 1", len(snaps))
	}
}

// Asserts no start is ever chained off a restore, on the succeeding path or the failing one:
// the operator looks at the world that came back before players do (B7, 12 §9.3).
func TestRestoreNeverStartsTheServer(t *testing.T) {
	tests := []struct {
		name  string
		seed  func(t *testing.T, w backupWorld) string
		state string
	}{
		{"succeeded", func(t *testing.T, w backupWorld) string {
			return takeBackup(t, w.rt, w.admin)
		}, "stopped"},
		{"failed", func(t *testing.T, w backupWorld) string {
			return seedUnrestorableArchive(t, w.db, w.rt.Supervisor().inst.Cfg.Data.Root)
		}, "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newBackupWorld(t, "stopped")
			backupID := tt.seed(t, w)

			runRestoreJob(t, w.rt, w.admin, backupID)

			if got := stateOf(t, w.db); got != tt.state {
				t.Errorf("state = %q, want %q", got, tt.state)
			}
			for _, j := range jobsOfKind(t, w.db, "start") {
				t.Errorf("a start job was submitted after a restore: %s", j)
			}
		})
	}
}

// jobsOfKind reports the ids of every job row of one kind.
func jobsOfKind(t *testing.T, db *store.DB, kind string) []string {
	t.Helper()
	rows, err := db.Reader.QueryContext(t.Context(), `SELECT id FROM job_runs WHERE kind = ?`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Asserts a restore of a running instance is refused and the server keeps running: the world
// it would replace is the one players are in (12 §3.1).
func TestRestoreOfARunningInstanceIsRefused(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	backupID := takeBackup(t, rt, admin)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = ?`, seededInstanceID)

	rec := postRestore(t, rt, admin, backupID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("restore of a running instance = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "instance_must_be_stopped" {
		t.Errorf("code = %q, want instance_must_be_stopped", got)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %q, want running", got)
	}
}

// Asserts a caller without backups.restore is refused, and one who cannot see the instance at
// all gets 404 rather than 403 (ADR-038).
func TestRestoreIsGatedOnBackupsRestore(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, admin, member := w.rt, w.admin, w.member
	backupID := takeBackup(t, rt, admin)

	if rec := postRestore(t, rt, member, backupID); rec.Code != http.StatusForbidden {
		t.Errorf("restore as a member without the grant = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec := as(rt, member, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/inst-nope/backups/"+backupID+"/restore", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("restore on an invisible instance = %d, want 404", rec.Code)
	}
}

// seedInterruptedSwap puts the instance's worlds directory into one of the states the two
// renames can be interrupted in, each directory carrying a marker naming it.
func seedInterruptedSwap(t *testing.T, db *store.DB, present ...string) string {
	t.Helper()
	root := worldsDirOf(t, db)
	for _, name := range present {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(name), 0o664); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Asserts a restore the panel was killed in the middle of is resolved to one world on the next
// boot, and that the instance is parked in `error` rather than started (B7, 12 §9.4).
func TestRecoverResolvesAnInterruptedRestoreSwap(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
	}{
		{"between the two renames", []string{"worlds_local.old", "worlds_local.new"}, "worlds_local.new"},
		{"before either rename", []string{"worlds_local", "worlds_local.new"}, "worlds_local"},
		{"after the second rename", []string{"worlds_local", "worlds_local.old"}, "worlds_local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, fake, _ := supervisorWorld(t)
			seedInstance(t, rt, db, fake, "restoring")
			root := seedInterruptedSwap(t, db, tt.present...)
			seedStaleJob(t, db, "restore", checkpointStaged, `{"backup_id":"b-1"}`)

			if err := rt.Supervisor().Recover(t.Context()); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(root, "worlds_local", "marker"))
			if err != nil {
				t.Fatalf("no world under worlds_local after recovery: %v", err)
			}
			if string(data) != tt.want {
				t.Errorf("worlds_local holds %q, want %q", data, tt.want)
			}
			for _, leftover := range []string{"worlds_local.new", "worlds_local.old"} {
				if _, err := os.Stat(filepath.Join(root, leftover)); !os.IsNotExist(err) {
					t.Errorf("%s survived recovery", leftover)
				}
			}
			if got := stateOf(t, db); got != "error" {
				t.Errorf("state = %q, want error (B7)", got)
			}
			for _, j := range jobsOfKind(t, db, "start") {
				t.Errorf("recovery started a server after an interrupted restore: %s", j)
			}
		})
	}
}

// Asserts a restore in flight cannot be cancelled: it replaces a world, and there is no
// interruptible half of that (12 §8).
func TestRestoreIsNeverCancellable(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	jobID := seedStaleJob(t, db, "restore", checkpointStaged, `{"backup_id":"b-1"}`)
	seed(t, db, `UPDATE job_runs SET lease_owner = ? WHERE id = ?`,
		rt.Supervisor().inst.Engine.Owner(), jobID)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/jobs/"+jobID+"/cancel", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel of a running restore = %d, want 409 (%s)", rec.Code, rec.Body)
	}
}
