package instance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/store/storetest"
)

// seededHistoryDB opens a database holding the instance inst-a.
func seededHistoryDB(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.Open(t)
	if _, err := db.Writer.ExecContext(t.Context(), `INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES ('inst-a', 'a', 'stopped', '/srv', 2456, 'S', 'W', 'enc', 'cp', ?, ?)`,
		store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestTrackConfigBeforeSkipsAKeptCopyTooLargeToKeep asserts a huge `.orig` left by an earlier
// release does not stop a small file's history.
func TestTrackConfigBeforeSkipsAKeptCopyTooLargeToKeep(t *testing.T) {
	t.Parallel()
	db := seededHistoryDB(t)
	dir := t.TempDir()
	big, err := os.Create(filepath.Join(dir, "a.cfg.orig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(store.MaxConfigRevisionBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = big.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := TrackConfigBefore(t.Context(), db, root, "inst-a", "a.cfg", []byte("small")); err != nil {
		t.Fatalf("track = %v, want the small file recorded", err)
	}
	revs, err := db.ConfigRevisions(t.Context(), "inst-a", "a.cfg")
	if err != nil || len(revs) != 1 || revs[0].Size != len("small") {
		t.Errorf("history = %+v, %v; want the small file alone", revs, err)
	}
}

// TestTrackConfigBeforeSkipsTheRCONFile asserts the RCON plugin's file, which holds the console
// password, never reaches the history, while any other file does.
func TestTrackConfigBeforeSkipsTheRCONFile(t *testing.T) {
	t.Parallel()
	db := seededHistoryDB(t)
	for _, name := range []string{command.ConfigFile, "other.cfg"} {
		if err := TrackConfigBefore(t.Context(), db, nil, "inst-a", name, []byte("Password = x\n")); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := db.LatestConfigRevisions(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := latest[command.ConfigFile]; ok || len(latest) != 1 {
		t.Errorf("history = %v, want other.cfg alone", latest)
	}
}
