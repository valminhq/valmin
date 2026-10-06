package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

func historyDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatal(err)
	}
	_, err = db.Writer.ExecContext(t.Context(), `INSERT INTO instances
		(id, name, state, data_dir, base_port, server_name, world_name, password,
		 crossplay_instance_id, created_at, updated_at)
		VALUES ('inst-a', 'inst-a', 'stopped', '/tmp/inst-a', 2456,
		 'Server', 'World', 'password', 'cp-inst-a', ?, ?)`, store.Now(), store.Now())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func historyLen(t *testing.T, db *store.DB) int {
	t.Helper()
	rows, err := db.ListPlayerObservations(t.Context(), "inst-a", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func intPtr(n int) *int { return &n }

func TestRecorderNeverBlocksReader(t *testing.T) {
	p := New(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range observationQueue * 2 {
			p.Observe("inst-a", instance.PlayerObservation{TS: time.Now(), Players: intPtr(1)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recorder blocked its caller when the queue filled")
	}
}

func TestRetentionSweepsOnFixedClock(t *testing.T) {
	db := historyDB(t)
	now := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	p := New(db)
	p.now = func() time.Time { return now }
	p.lastPrune = now
	stale := now.Add(-Retention - time.Hour)
	if err := db.RecordPlayerObservation(t.Context(), "inst-a", stale, intPtr(1)); err != nil {
		t.Fatal(err)
	}
	p.write(
		context.Background(),
		recorded{instanceID: "inst-a", obs: instance.PlayerObservation{TS: now, Players: intPtr(2)}},
	)
	if got := historyLen(t, db); got != 2 {
		t.Fatalf("inside interval: %d rows, want 2", got)
	}
	now = now.Add(pruneInterval + time.Minute)
	p.write(
		context.Background(),
		recorded{instanceID: "inst-a", obs: instance.PlayerObservation{TS: now, Players: intPtr(3)}},
	)
	if got := historyLen(t, db); got != 2 {
		t.Fatalf("after sweep: %d rows, want 2", got)
	}
}

func TestUnstampedObservationUsesClock(t *testing.T) {
	db := historyDB(t)
	now := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	p := New(db)
	p.now = func() time.Time { return now }
	p.lastPrune = now
	p.write(context.Background(), recorded{instanceID: "inst-a"})
	rows, err := db.ListPlayerObservations(t.Context(), "inst-a", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ObservedAt.Equal(now) || rows[0].Players != nil {
		t.Fatalf("recorded %v, want a gap stamped %s", rows, now)
	}
}

func TestIdentitySightingsMerge(t *testing.T) {
	db := historyDB(t)
	p := New(db)
	base := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	for _, id := range []instance.PlayerIdentity{
		{TS: base, PlatformID: "steam-id"},
		{TS: base.Add(time.Minute), PlatformID: "steam-id", Name: "Player"},
		{TS: base.Add(2 * time.Minute), PlatformID: "steam-id"},
	} {
		p.writeIdentity(t.Context(), "inst-a", id)
	}
	rows, err := db.ListPlayerIdentities(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "Player" || !rows[0].LastSeenAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("merged sightings: %+v", rows)
	}
}
