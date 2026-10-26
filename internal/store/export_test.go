package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCopyToWritesAConsistentDatabase(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedInstance(t, db, "server-a", 28000)

	path := filepath.Join(t.TempDir(), "panel.db")
	if err := db.CopyTo(t.Context(), path); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	db.Reader.SetMaxOpenConns(1)
	if _, err := db.Reader.ExecContext(t.Context(), `DELETE FROM instances`); err == nil {
		t.Error("a reader connection accepted a write after the copy")
	}
	copied, err := Open(t.Context(), "sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	inst, err := copied.InstanceByID(t.Context(), "server-a")
	if err != nil || inst == nil {
		t.Fatalf("InstanceByID in the copy = %+v, %v", inst, err)
	}
}

// Asserts a restored database names no archive the export left out, and that a setup which
// named one keeps everything else.
func TestPrepareRestoredDropsMissingArchivesAndStopsServers(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedUser(t, db, "admin")
	seedInstance(t, db, "server-a", 28000)
	exec(t, db.Writer, `UPDATE instances SET state = 'running'`)
	seedInstance(t, db, "server-b", 28010)
	exec(t, db.Writer, `UPDATE instances SET state = 'error' WHERE id = 'server-b'`)
	for _, id := range []string{"kept", "gone"} {
		b := Backup{
			ID: id, InstanceID: "server-a", Path: "/tmp/" + id, SHA256: "hash",
			WorldName: "Worldserver-a", Trigger: TriggerManual, Consistent: true,
		}
		if err := db.CreateBackup(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
	}
	setup := &SavedSetup{
		ID: "setup", InstanceID: "server-a", Name: "Working", CreatedBy: "admin",
		GameBuildID: "build", WorldName: "Worldserver-a", SnapshotJSON: "{}", BackupID: "gone",
	}
	if err := db.SaveSetup(t.Context(), setup, nil); err != nil {
		t.Fatal(err)
	}
	export := newJob(NewID(), "export", "global:export")
	if err := db.ClaimJobWithLocks(t.Context(), export, []string{"instance:server-a"}, "panel:old-host",
		time.Now().Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}

	builds := map[string]string{"server-a": "20512345"}
	if err := db.PrepareRestored(t.Context(), builds, []BackupFile{{InstanceID: "server-a", ID: "gone"}}); err != nil {
		t.Fatalf("PrepareRestored: %v", err)
	}

	files, err := db.BackupFiles(t.Context())
	if err != nil || len(files) != 1 || files[0].ID != "kept" {
		t.Errorf("BackupFiles = %+v, %v, want only kept", files, err)
	}
	got, _, err := db.SetupByID(t.Context(), "server-a", "setup")
	if err != nil || got == nil || got.BackupID != "" {
		t.Errorf("setup = %+v, %v, want it kept without its archive", got, err)
	}
	inst, err := db.InstanceByID(t.Context(), "server-a")
	if err != nil || inst.State != "stopped" || inst.GameBuildID == nil || *inst.GameBuildID != "20512345" {
		t.Errorf("instance = %+v, %v, want stopped on build 20512345", inst, err)
	}
	parked, err := db.InstanceByID(t.Context(), "server-b")
	if err != nil || parked.State != "error" {
		t.Errorf("parked instance = %+v, %v, want left in error", parked, err)
	}
	job, err := db.JobByID(t.Context(), export.ID)
	if err != nil || job.Status != "succeeded" {
		t.Errorf("export job = %+v, %v, want succeeded", job, err)
	}
	if _, held, err := db.LockHolder(t.Context(), "instance:server-a"); err != nil || held {
		t.Errorf("instance lock held = %v, %v, want released", held, err)
	}
}
