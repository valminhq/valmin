package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// stopArchivePayload is the payload a stop or restart job holds once an archive is planned.
func stopArchivePayload(t *testing.T, id, path string) string {
	t.Helper()
	raw, err := json.Marshal(ArchivePlan{ArchiveID: id, ArchivePath: path})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// writeFile creates path, and its directory, holding some bytes.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether path is on disk.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestTheSweepCleansWhatAnInterruptedStopArchiveLeft asserts that when the panel died during the
// archive a stop or restart job was writing, the next start removes the partial file, and the
// published file when no catalogue row names it, and nothing else.
func TestTheSweepCleansWhatAnInterruptedStopArchiveLeft(t *testing.T) {
	for _, kind := range []jobs.Kind{jobs.KindStop, jobs.KindRestart} {
		t.Run(kind.String(), func(t *testing.T) {
			w, db, _, _ := supervisorWorld(t)
			seedInstance(t, w, "stopping")
			root := instance.BackupsDir(w.cfg.Data.Root)
			dest := filepath.Join(root, seededInstanceID, "Server-20261113T200000Z-abc123.tar.gz")
			bystander := filepath.Join(root, seededInstanceID, "Server-20261112T100000Z-def456.tar.gz")
			writeFile(t, dest+backup.PartSuffix)
			writeFile(t, dest)
			writeFile(t, bystander)
			seedStaleJob(t, db, kind.String(), "", stopArchivePayload(t, "arc-1", dest))

			if _, err := w.c.Supervisor.Sweep(t.Context()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if exists(dest + backup.PartSuffix) {
				t.Error("the partial archive survived the sweep")
			}
			if exists(dest) {
				t.Error("the published archive no row names survived the sweep")
			}
			if !exists(bystander) {
				t.Error("the sweep removed a file the interrupted job never wrote")
			}
		})
	}
}

// TestTheSweepKeepsAStopArchiveTheCatalogueNames asserts a published archive that has a row, the
// job having committed it, is left alone.
func TestTheSweepKeepsAStopArchiveTheCatalogueNames(t *testing.T) {
	w, db, _, _ := supervisorWorld(t)
	seedInstance(t, w, "stopping")
	dest := filepath.Join(
		instance.BackupsDir(w.cfg.Data.Root),
		seededInstanceID,
		"Server-20261113T200000Z-abc123.tar.gz",
	)
	writeFile(t, dest)
	if err := db.CreateBackup(t.Context(), &store.Backup{
		ID: "arc-1", InstanceID: seededInstanceID, Path: dest, SizeBytes: 13,
		SHA256: "00", WorldName: "World", Trigger: store.TriggerManual, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	seedStaleJob(t, db, jobs.KindStop.String(), "", stopArchivePayload(t, "arc-1", dest))

	if _, err := w.c.Supervisor.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !exists(dest) {
		t.Error("the sweep removed an archive the catalogue names")
	}
}

// TestTheSweepRefusesAStopArchiveOutsideTheBackupsRoot asserts a path read back from the database
// is removed only when it lies in the instance's own backup directory.
func TestTheSweepRefusesAStopArchiveOutsideTheBackupsRoot(t *testing.T) {
	w, db, _, _ := supervisorWorld(t)
	seedInstance(t, w, "stopping")
	root := instance.BackupsDir(w.cfg.Data.Root)
	elsewhere := filepath.Join(w.cfg.Data.Root, "instances", "keep-me.tar.gz")
	otherInstance := filepath.Join(root, "inst-b", "Other-20261113T200000Z-abc123.tar.gz")
	writeFile(t, elsewhere)
	writeFile(t, otherInstance)

	seedStaleJob(t, db, jobs.KindStop.String(), "", stopArchivePayload(t, "arc-1", elsewhere))
	seed(t, db, `DELETE FROM job_locks`)
	seedStaleJob(t, db, jobs.KindRestart.String(), "", stopArchivePayload(t, "arc-2", otherInstance))

	if _, err := w.c.Supervisor.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !exists(elsewhere) {
		t.Error("the sweep removed a file outside the backups root")
	}
	if !exists(otherInstance) {
		t.Error("the sweep removed another instance's archive")
	}
}

// TestTheSweepRemovesOnlyFilesDirectlyInTheInstancesBackupDirectory asserts a planned path that
// is deeper than the archive directory the panel writes to is left alone, even inside the
// instance's own folder.
func TestTheSweepRemovesOnlyFilesDirectlyInTheInstancesBackupDirectory(t *testing.T) {
	w, db, _, _ := supervisorWorld(t)
	seedInstance(t, w, "stopping")
	nested := filepath.Join(
		instance.BackupsDir(w.cfg.Data.Root),
		seededInstanceID,
		"nested",
		"Server-20261113T200000Z-abc123.tar.gz",
	)
	writeFile(t, nested)
	seedStaleJob(t, db, jobs.KindStop.String(), "", stopArchivePayload(t, "arc-1", nested))

	if _, err := w.c.Supervisor.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !exists(nested) {
		t.Error("the sweep removed a file below the instance's archive directory")
	}
}

// TestTheSweepIgnoresAStopThatPlannedNoArchive asserts a stop or restart job with no archive in
// its payload, or a payload that is not JSON, sweeps without removing anything or failing.
func TestTheSweepIgnoresAStopThatPlannedNoArchive(t *testing.T) {
	for _, payload := range []string{`{"reason":"no_players"}`, `{}`, `not json`} {
		t.Run(payload, func(t *testing.T) {
			w, db, _, _ := supervisorWorld(t)
			seedInstance(t, w, "stopping")
			keep := filepath.Join(
				instance.BackupsDir(w.cfg.Data.Root),
				seededInstanceID,
				"Server-20261112T100000Z-def456.tar.gz",
			)
			writeFile(t, keep)
			seedStaleJob(t, db, jobs.KindStop.String(), "", payload)

			if _, err := w.c.Supervisor.Sweep(t.Context()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if !exists(keep) {
				t.Error("the sweep removed an archive with no job to blame")
			}
		})
	}
}
