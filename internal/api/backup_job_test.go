package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// seedWorldOnDisk writes a world large enough to clear backup.Verify's floors, so an archive
// of it is one.
func seedWorldOnDisk(t *testing.T, db *store.DB) {
	t.Helper()
	dir := filepath.Join(worldsDirOf(t, db), "worlds_local")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "World.db"), dbBytes(), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "World.fwl"), fwlBytes(37, "World"), 0o664); err != nil {
		t.Fatal(err)
	}
}

// backupWorld is a seeded panel: one instance in a known state, with a real world on disk.
type backupWorld struct {
	rt            *Router
	db            *store.DB
	fake          *runtime.Fake
	containerID   string
	admin, member *store.User
}

func newBackupWorld(t *testing.T, state string) backupWorld {
	t.Helper()
	rt, db, fake, admin, member := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, state)
	seedWorldOnDisk(t, db)
	return backupWorld{rt, db, fake, containerID, admin, member}
}

func postBackup(t *testing.T, rt *Router, u *store.User, query string) jobView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodPost, backupsPath+query, http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create backup = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	return stub
}

// waitUntilRunning polls until the instance is running, since a job's chained start finishes
// after the job itself does.
func waitUntilRunning(t *testing.T, db *store.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = stateOf(t, db); got == "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state = %q, want running", got)
}

func archiveFiles(t *testing.T, rt *Router) []string {
	t.Helper()
	dir := filepath.Join(rt.Supervisor().inst.Cfg.Data.Root, "backups", "inst-a")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Asserts a backup of a stopped instance archives, records a consistent row, and leaves the
// instance stopped without entering a transient state it never needed.
func TestBackupOfAStoppedInstanceRecordsAConsistentArchive(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin

	stub := postBackup(t, rt, admin, "")
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("backup job = %+v", final)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}

	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 1 {
		t.Fatalf("catalogue has %d archives, want 1", len(page.Items))
	}
	if page.Items[0]["consistent"] != true {
		t.Error("an archive of a stopped server is not marked consistent")
	}
	if files := archiveFiles(t, rt); len(files) != 1 {
		t.Errorf("backups/ holds %v, want exactly the one archive", files)
	}
}

// Asserts the quiesced sequence of 12 §2.3 over a running server: it stops, archives, and is
// started again by the job's own resume.
func TestBackupOfARunningInstanceStopsArchivesAndStartsAgain(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, fake, containerID, admin := w.rt, w.db, w.fake, w.containerID, w.admin
	fake.Get(containerID).Stdout("World save writing finished\n")

	stub := postBackup(t, rt, admin, "")
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("backup job = %+v", final)
	}

	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 1 || page.Items[0]["consistent"] != true {
		t.Fatalf("catalogue = %v, want one consistent archive", page.Items)
	}
	waitUntilRunning(t, db)
}

// Asserts a quiesce that never saw the save line writes no archive and no row. 12 §3.4: a
// backup that degrades to a hot copy when the quiesce fails is worse than no backup, because
// the catalogue then holds a file marked consistent that is not.
func TestBackupRefusesToArchiveWhenTheSaveWasNotConfirmed(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	// Deliberately no "World save writing finished" in the container's log.

	stub := postBackup(t, rt, admin, "")
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("backup job = %+v, want failed", final)
	}
	if final.ErrorCode == nil || *final.ErrorCode != "backup_unverifiable" {
		t.Errorf("error_code = %v, want backup_unverifiable", final.ErrorCode)
	}

	if page := listBackupsAs(t, rt, admin, ""); len(page.Items) != 0 {
		t.Errorf("the catalogue holds %d archives after a failed quiesce, want none", len(page.Items))
	}
	if files := archiveFiles(t, rt); len(files) != 0 {
		t.Errorf("backups/ holds %v after a failed quiesce, want nothing", files)
	}
	// The operator asked for a backup, not a shutdown.
	waitUntilRunning(t, db)
}

// Asserts a hot copy never stops the server, never enters a transient state, and is recorded
// as best-effort (B12).
func TestHotCopyLeavesTheServerRunningAndIsNotConsistent(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, fake, containerID, admin := w.rt, w.db, w.fake, w.containerID, w.admin

	stub := postBackup(t, rt, admin, "?mode=hot")
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("hot copy job = %+v", final)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %q, want running throughout", got)
	}
	if c := fake.Get(containerID); c == nil || !c.Running {
		t.Error("a hot copy stopped the container")
	}

	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 1 {
		t.Fatalf("catalogue has %d archives, want 1", len(page.Items))
	}
	if page.Items[0]["consistent"] != false {
		t.Error("a hot copy of a running server is marked consistent")
	}
}

// Asserts an archive that does not verify is refused, records nothing, and is removed rather
// than left where a later operator reads it as a backup.
func TestBackupRefusesAnArchiveThatDoesNotVerify(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	// Emptied after seeding, so Archive succeeds over a world Verify will not vouch for.
	if err := os.WriteFile(
		filepath.Join(worldsDirOf(t, db), "worlds_local", "World.db"), nil, 0o664); err != nil {
		t.Fatal(err)
	}

	stub := postBackup(t, rt, admin, "")
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("backup job = %+v, want failed", final)
	}
	if final.ErrorCode == nil || *final.ErrorCode != "backup_unverifiable" {
		t.Errorf("error_code = %v, want backup_unverifiable", final.ErrorCode)
	}
	if page := listBackupsAs(t, rt, admin, ""); len(page.Items) != 0 {
		t.Errorf("the catalogue holds %d archives, want none", len(page.Items))
	}
	if files := archiveFiles(t, rt); len(files) != 0 {
		t.Errorf("backups/ holds %v, want nothing: an unverified archive was left behind", files)
	}
}

// Asserts repeated backups are trimmed to keep_cold, files and rows together. The cross-class
// property — that hot copies cannot evict a cold archive — is TestPruneNeverLetsHotCopies-
// EvictAColdArchive in internal/backup.
func TestBackupPrunesToTheColdRetentionCount(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, admin := w.rt, w.db, w.admin
	seed(t, db, `UPDATE instances SET backup_keep_cold = 2, backup_keep_hot = 1 WHERE id = 'inst-a'`)

	for range 4 {
		stub := postBackup(t, rt, admin, "")
		if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
			t.Fatalf("backup job = %+v", final)
		}
	}
	page := listBackupsAs(t, rt, admin, "")
	if len(page.Items) != 2 {
		t.Fatalf("catalogue has %d archives after four backups with keep_cold=2, want 2", len(page.Items))
	}
	if files := archiveFiles(t, rt); len(files) != 2 {
		t.Errorf("backups/ holds %v, want the two kept archives — pruned rows left their files", files)
	}
}

func TestBackupNeedsTheCreateAction(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, member := w.rt, w.member

	rec := as(rt, member, httptest.NewRequest(http.MethodPost, backupsPath, http.NoBody))
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer backup = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

func TestBackupRefusesAnUnknownMode(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, admin := w.rt, w.admin

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, backupsPath+"?mode=fast", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown mode = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

// Asserts backup_on_restart archives on the way through `stopped` and leaves the instance
// running, and that it does nothing when the instance has not opted in.
func TestRestartArchivesOnlyWhenTheInstanceOptsIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"opted in", true, 1},
		{"not opted in", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBackupWorld(t, "running")
			rt, db, fake, containerID, admin := w.rt, w.db, w.fake, w.containerID, w.admin
			seed(t, db, `UPDATE instances SET backup_on_restart = ? WHERE id = 'inst-a'`, tc.enabled)
			fake.Get(containerID).Stdout("World save writing finished\n")

			rec := as(rt, admin, httptest.NewRequest(
				http.MethodPost, "/api/v1/instances/inst-a/restart", http.NoBody))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("restart = %d, want 202 (%s)", rec.Code, rec.Body)
			}
			var stub jobView
			decodeInto(t, rec, &stub)
			if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
				t.Fatalf("restart job = %+v", final)
			}

			page := listBackupsAs(t, rt, admin, "")
			if len(page.Items) != tc.want {
				t.Fatalf("catalogue has %d archives, want %d", len(page.Items), tc.want)
			}
			if tc.want == 1 && page.Items[0]["consistent"] != true {
				t.Error("a restart archive taken after a confirmed save is not marked consistent")
			}
			waitUntilRunning(t, db)
		})
	}
}

// Asserts a restart whose stop never confirmed the save takes no archive and still starts the
// server. The archive is opportunistic; the restart is not (12 §3.4).
func TestRestartTakesNoArchiveWhenTheSaveWasNotConfirmed(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	seed(t, db, `UPDATE instances SET backup_on_restart = TRUE WHERE id = 'inst-a'`)
	// Deliberately no save line.

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/inst-a/restart", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("restart job = %+v, want succeeded: the archive is opportunistic", final)
	}

	if page := listBackupsAs(t, rt, admin, ""); len(page.Items) != 0 {
		t.Errorf("the catalogue holds %d archives after an unconfirmed save, want none", len(page.Items))
	}
	waitUntilRunning(t, db)
}

// Asserts a restart whose archive fails verification still starts the server, writes no
// catalogue row, leaves no file behind, and says on the job why there is no archive. This is
// the branch the save-line case does not reach: the save was confirmed and the archive was
// taken, and it is Verify that refuses it. A restart is not opportunistic (12 §3.4), so a
// failure here costs the archive and nothing else.
func TestARestartWhoseArchiveFailsVerificationStillStartsTheServer(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, fake, containerID, admin := w.rt, w.db, w.fake, w.containerID, w.admin
	seed(t, db, `UPDATE instances SET backup_on_restart = TRUE WHERE id = 'inst-a'`)
	fake.Get(containerID).Stdout("World save writing finished\n")
	// Emptied after seeding, so Archive succeeds over a world Verify will not vouch for.
	if err := os.WriteFile(
		filepath.Join(worldsDirOf(t, db), "worlds_local", "World.db"), nil, 0o664); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/inst-a/restart", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "succeeded" {
		t.Fatalf("restart job = %+v, want succeeded: an unverifiable archive must not fail it", final)
	}

	if page := listBackupsAs(t, rt, admin, ""); len(page.Items) != 0 {
		t.Errorf("the catalogue holds %d archives, want none: an unverified archive was recorded",
			len(page.Items))
	}
	if files := archiveFiles(t, rt); len(files) != 0 {
		t.Errorf("backups/ holds %v, want nothing: an unverified archive was left behind", files)
	}

	var log *string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT log FROM job_runs WHERE id = ?`, stub.JobID).Scan(&log); err != nil {
		t.Fatal(err)
	}
	if log == nil || !strings.Contains(*log, "no archive was taken on this restart") {
		t.Errorf("the job log does not say why there is no archive: %v", log)
	}
	waitUntilRunning(t, db)
}
