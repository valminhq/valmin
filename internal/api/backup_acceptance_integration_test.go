//go:build integration

package api

import (
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

func TestScheduledBackupsRetainColdArchivesAcrossHotCopies(t *testing.T) {
	rt, db, docker, admin := lifecycleRouter(t)
	seedRealInstance(t, rt, db, docker, seededInstanceID)
	seedWorldOnDisk(t, db)
	seed(t, db, `UPDATE instances SET backup_keep_cold = 2, backup_keep_hot = 5 WHERE id = ?`, seededInstanceID)

	now := time.Date(2030, time.January, 1, 3, 0, 0, 0, time.UTC)
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "backup", &id, now)
	clock := &scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}
	var cold []store.Backup
	for run := range 4 {
		clock.Tick(t.Context(), now.Add(time.Duration(run)*24*time.Hour))
		rows := jobRowsForSchedule(t, db, scheduleID)
		if len(rows) != run+1 {
			t.Fatalf("scheduled run %d: got %d jobs, want %d", run+1, len(rows), run+1)
		}
		job := rows[0]
		if job.RequestedBy != nil || job.Kind != "backup" {
			t.Fatalf("scheduled run has a requester or wrong kind: %+v", job)
		}
		if final := waitForJobTerminal(t, rt, admin, job.ID); final.Status != "succeeded" {
			t.Fatalf("scheduled backup = %+v", final)
		}
		archives := acceptanceArchives(t, db)
		if len(archives) != min(run+1, 2) {
			t.Fatalf("run %d retained %d archives", run+1, len(archives))
		}
		newest := archives[0]
		if !newest.Consistent || newest.Trigger != store.TriggerScheduled {
			t.Fatalf("scheduled archive = %+v", newest)
		}
		cold = append(cold, newest)
	}
	for _, old := range cold[:2] {
		assertArchiveRemoved(t, db, old)
	}
	if start := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start"); start.Status != "succeeded" {
		t.Fatalf("start = %+v", start)
	}
	for range 20 {
		stub := postBackup(t, rt, admin, "?mode=hot")
		if final := waitForJobTerminal(t, rt, admin, stub.JobID); final.Status != "succeeded" {
			t.Fatalf("hot copy = %+v", final)
		}
		if got := instanceState(t, rt, admin, id); got != "running" {
			t.Fatalf("hot copy left instance %q", got)
		}
	}
	archives := acceptanceArchives(t, db)
	var hotCount, coldCount int
	for _, archive := range archives {
		if archive.Consistent {
			coldCount++
		} else {
			hotCount++
		}
	}
	if hotCount != 5 || coldCount != 2 {
		t.Fatalf("retained hot=%d cold=%d, want hot=5 cold=2", hotCount, coldCount)
	}
	for _, retained := range cold[2:] {
		row, err := db.BackupByID(t.Context(), id, retained.ID)
		if err != nil || row == nil {
			t.Fatalf("cold archive %s lost after hot copies: row=%v err=%v", retained.ID, row, err)
		}
	}
	if files := archiveFiles(t, rt); len(files) != 7 {
		t.Fatalf("archive directory has %d files, want 7: %v", len(files), files)
	}
}

func acceptanceArchives(t *testing.T, db *store.DB) []store.Backup {
	t.Helper()
	rows, err := db.ListBackups(t.Context(), seededInstanceID, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, err := backup.Verify(row.Path, seededWorldName); err != nil {
			t.Fatalf("verify catalogued archive %s: %v", row.ID, err)
		}
	}
	return rows
}

func assertArchiveRemoved(t *testing.T, db *store.DB, archive store.Backup) {
	t.Helper()
	row, err := db.BackupByID(t.Context(), seededInstanceID, archive.ID)
	if err != nil || row != nil {
		t.Fatalf("pruned archive %s remains in catalogue: row=%v err=%v", archive.ID, row, err)
	}
	if _, err := os.Stat(archive.Path); !os.IsNotExist(err) {
		t.Fatalf("pruned archive %s remains on disk: %v", archive.ID, err)
	}
}

func TestRestoreCorruptedWorldThroughAPI(t *testing.T) {
	rt, db, docker, admin := lifecycleRouter(t)
	seedRealInstance(t, rt, db, docker, seededInstanceID)
	seedWorldOnDisk(t, db)
	root := worldsDirOf(t, db)
	for _, name := range []string{"World.db.old", "saved/World_backup.db", "saved/World_backup.fwl"} {
		path := filepath.Join(root, "worlds_local", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("saved world: "+name), 0o664); err != nil {
			t.Fatal(err)
		}
	}
	before := acceptanceWorldTree(t, root)
	backupID := takeBackup(t, rt, admin)
	corruptTheWorld(t, db)
	corrupted := acceptanceWorldTree(t, root)
	if maps.Equal(corrupted, before) {
		t.Fatal("corruption did not change the world")
	}
	if final := runRestoreJob(t, rt, admin, backupID); final.Status != "succeeded" {
		t.Fatalf("restore = %+v", final)
	}
	if got := acceptanceWorldTree(t, root); !maps.Equal(got, before) {
		t.Fatal("restored world tree differs from the archived tree")
	}
	if state := instanceState(t, rt, admin, seededInstanceID); state != "stopped" {
		t.Fatalf("restore left instance %q, want stopped", state)
	}
	snapshots := backupsWithTrigger(t, rt, admin, store.TriggerPreRestore)
	if len(snapshots) != 1 {
		t.Fatalf("got %d pre-restore snapshots, want 1", len(snapshots))
	}
	snapshot, err := db.BackupByID(t.Context(), seededInstanceID, snapshots[0])
	if err != nil || snapshot == nil {
		t.Fatalf("read pre-restore snapshot: row=%v err=%v", snapshot, err)
	}
	extracted := t.TempDir()
	if err := backup.Extract(snapshot.Path, "", extracted); err != nil {
		t.Fatalf("extract pre-restore snapshot: %v", err)
	}
	if got := acceptanceWorldTree(t, extracted); !maps.Equal(got, corrupted) {
		t.Fatal("pre-restore snapshot differs from the corrupted world it should preserve")
	}
}

func acceptanceWorldTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[name+"/"] = ""
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[name] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
