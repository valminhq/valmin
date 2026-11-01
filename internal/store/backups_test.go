package store

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/backup"
)

// worldCheckEpoch anchors the archive times these tests write, a minute apart.
var worldCheckEpoch = time.Date(2026, 10, 21, 19, 0, 0, 0, time.UTC)

// seedArchive records an archive taken minute minutes after worldCheckEpoch. A negative
// worldBytes leaves the size unmeasured; a non-empty attention flags it.
func seedArchive(
	t *testing.T, db *DB, id, trigger string, minute int, worldBytes int64, attention ...backup.Attention,
) {
	t.Helper()
	b := &Backup{
		ID: id, InstanceID: "i1", Path: "/srv/" + id + ".tar.gz", SizeBytes: 1, SHA256: "abc",
		WorldName: "Builders", Trigger: trigger, Consistent: true, WorldAttention: attention,
	}
	if worldBytes >= 0 {
		b.WorldBytes = &worldBytes
	}
	if err := db.CreateBackup(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	exec(t, db.Writer, `UPDATE backups SET created_at = ? WHERE id = ?`,
		FormatTime(worldCheckEpoch.Add(time.Duration(minute)*time.Minute)), id)
}

var damaged = backup.Attention{Code: backup.AttentionChunkMissing, Message: "a chunk is missing"}

func TestBackupRoundTripsTheWorldCheck(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedInstance(t, db, "i1", 2456)
	seedArchive(t, db, "b1", TriggerManual, 0, 4096, damaged)
	seedArchive(t, db, "b2", TriggerManual, 1, -1)

	b1, err := db.BackupByID(t.Context(), "i1", "b1")
	if err != nil {
		t.Fatal(err)
	}
	if b1.WorldBytes == nil || *b1.WorldBytes != 4096 || len(b1.WorldAttention) != 1 ||
		b1.WorldAttention[0] != damaged {
		t.Errorf("b1 read back as bytes=%v attention=%+v", b1.WorldBytes, b1.WorldAttention)
	}
	b2, err := db.BackupByID(t.Context(), "i1", "b2")
	if err != nil {
		t.Fatal(err)
	}
	if b2.WorldBytes != nil || b2.WorldAttention != nil {
		t.Errorf("an unmeasured archive read back as bytes=%v attention=%+v", b2.WorldBytes, b2.WorldAttention)
	}
}

func TestWorldBaseline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seed   func(t *testing.T, db *DB)
		want   int64
		wantOK bool
	}{
		{"the newest archive that passed", func(t *testing.T, db *DB) {
			seedArchive(t, db, "old", TriggerScheduled, 0, 100)
			seedArchive(t, db, "new", TriggerScheduled, 1, 200)
		}, 200, true},
		// A broken world archived every day stays compared against the last one that passed,
		// rather than becoming its own baseline.
		{"flagged archives are skipped", func(t *testing.T, db *DB) {
			seedArchive(t, db, "good", TriggerScheduled, 0, 1000)
			seedArchive(t, db, "bad1", TriggerScheduled, 1, 300, damaged)
			seedArchive(t, db, "bad2", TriggerScheduled, 2, 300, damaged)
		}, 1000, true},
		{"unmeasured archives are skipped", func(t *testing.T, db *DB) {
			seedArchive(t, db, "measured", TriggerScheduled, 0, 1000)
			seedArchive(t, db, "legacy", TriggerScheduled, 1, -1)
		}, 1000, true},
		// Restore, import, mod updates and world tools take a snapshot first: the world changed
		// on purpose, so nothing before it is a fair comparison.
		{"a snapshot starts the comparison again", func(t *testing.T, db *DB) {
			seedArchive(t, db, "before", TriggerScheduled, 0, 1000)
			seedArchive(t, db, "snap", TriggerPreUpdate, 1, -1)
		}, 0, false},
		{"archives after the snapshot count", func(t *testing.T, db *DB) {
			seedArchive(t, db, "before", TriggerScheduled, 0, 1000)
			seedArchive(t, db, "snap", TriggerPreRestore, 1, -1)
			seedArchive(t, db, "after", TriggerManual, 2, 400)
		}, 400, true},
		{"an acknowledged archive passes", func(t *testing.T, db *DB) {
			seedArchive(t, db, "good", TriggerScheduled, 0, 1000)
			seedArchive(t, db, "reset", TriggerScheduled, 1, 300, damaged)
			if ok, err := db.AcknowledgeWorld(t.Context(), "i1", "reset", nil); err != nil || !ok {
				t.Fatalf("AcknowledgeWorld = %v, %v", ok, err)
			}
		}, 300, true},
		{"no archives", func(*testing.T, *DB) {}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := open(t)
			seedInstance(t, db, "i1", 2456)
			tc.seed(t, db)
			got, _, ok, err := db.WorldBaseline(t.Context(), "i1", "Builders")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("WorldBaseline = %d, %v, want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestWorldBaselineIgnoresAnotherWorld(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedInstance(t, db, "i1", 2456)
	seedArchive(t, db, "b1", TriggerScheduled, 0, 1000)

	if _, _, ok, err := db.WorldBaseline(t.Context(), "i1", "Another"); err != nil || ok {
		t.Errorf("WorldBaseline(Another) = %v, %v, want no baseline", ok, err)
	}
}

func TestAcknowledgeWorldReportsAnUnknownArchive(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedInstance(t, db, "i1", 2456)
	if ok, err := db.AcknowledgeWorld(t.Context(), "i1", "missing", nil); err != nil || ok {
		t.Errorf("AcknowledgeWorld(missing) = %v, %v, want false", ok, err)
	}
}

func TestFlaggedLatestIsTheNewestArchiveOnly(t *testing.T) {
	t.Parallel()
	db := open(t)
	seedInstance(t, db, "i1", 2456)
	seedArchive(t, db, "bad", TriggerScheduled, 0, 300, damaged)
	seedArchive(t, db, "snap", TriggerPreUpdate, 1, -1)

	got, err := db.FlaggedLatest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got["i1"].ID != "bad" {
		t.Fatalf("FlaggedLatest = %+v, want the flagged archive, a later snapshot not hiding it", got)
	}

	seedArchive(t, db, "good", TriggerScheduled, 2, 300)
	if got, err = db.FlaggedLatest(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("FlaggedLatest = %+v, %v, want nothing once a newer archive passed", got, err)
	}
}
