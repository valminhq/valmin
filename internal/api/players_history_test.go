package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

func historyPath() string { return "/api/v1/instances/inst-a/players/history" }

// TestHistoryReportsGapsAsNull. The whole reason the column is nullable: a client that reads
// a gap as zero draws an empty server where the panel only meant it had stopped looking.
func TestHistoryReportsGapsAsNull(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	base := time.Date(2026, 9, 8, 8, 30, 0, 0, time.UTC)
	for i, players := range []*int{intPtr(0), intPtr(1), nil} {
		if err := db.RecordPlayerObservation(
			t.Context(), "inst-a", base.Add(time.Duration(i)*time.Minute), players); err != nil {
			t.Fatal(err)
		}
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, historyPath(), http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET history = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page Page[playerObservationView]
	decodeInto(t, rec, &page)

	if len(page.Items) != 3 {
		t.Fatalf("read back %d points, want 3", len(page.Items))
	}
	if page.Items[0].Players != nil {
		t.Errorf("the newest point is %d, want null", *page.Items[0].Players)
	}
	if page.Items[2].Players == nil || *page.Items[2].Players != 0 {
		t.Errorf("the oldest point is %v, want a real 0", page.Items[2].Players)
	}
}

// TestHistoryPagesWithAStableCursor walks the whole history two points at a time and asserts
// nothing repeats or vanishes at a boundary.
func TestHistoryPagesWithAStableCursor(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	// One timestamp for all five: the count can change twice inside Docker's one-second
	// resolution, and the id is what breaks the tie (ADR-035).
	same := time.Date(2026, 9, 8, 8, 30, 0, 0, time.UTC)
	for i := range 5 {
		if err := db.RecordPlayerObservation(t.Context(), "inst-a", same, intPtr(i)); err != nil {
			t.Fatal(err)
		}
	}

	seen, url := map[string]bool{}, historyPath()+"?limit=2"
	for range 5 {
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, url, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", url, rec.Code, rec.Body)
		}
		var page Page[playerObservationView]
		decodeInto(t, rec, &page)
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("point %s came back on two pages", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		url = historyPath() + "?limit=2&cursor=" + *page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged over %d of 5 points", len(seen))
	}
}

// TestHistoryOfAnUnseenInstanceIs404 is ADR-038: a caller with no grant learns nothing about
// whether the instance exists, on this route as on every other.
func TestHistoryOfAnUnseenInstanceIs404(t *testing.T) {
	rt, db, fake, _, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-nope/players/history", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unseen instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestTheRecorderNeverBlocksTheReader is C21. The log reader hands observations over on the
// read loop, so a full queue drops rather than waiting: history is lossy, the console is not.
func TestTheRecorderNeverBlocksTheReader(t *testing.T) {
	p := NewPlayerRecorder(nil)
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
		t.Fatal("the recorder blocked its caller once the queue filled")
	}
}

// TestRetentionSweepsOnAFixedClock. The sweep is checked on a write rather than driven by a
// ticker, so a panel nobody plays on does no work; this pins the interval and the horizon.
func TestRetentionSweepsOnAFixedClock(t *testing.T) {
	rt, db, fake, _, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	now := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	p := NewPlayerRecorder(db)
	p.now = func() time.Time { return now }
	p.lastPrune = now

	stale := now.Add(-PlayerHistoryRetention - time.Hour)
	if err := db.RecordPlayerObservation(t.Context(), "inst-a", stale, intPtr(1)); err != nil {
		t.Fatal(err)
	}

	// Inside the interval nothing is swept, however old the rows are.
	p.write(context.Background(), recorded{
		instanceID: "inst-a",
		obs:        instance.PlayerObservation{TS: now, Players: intPtr(2)},
	})
	if got := historyLen(t, db); got != 2 {
		t.Fatalf("%d rows after a write inside the sweep interval, want 2", got)
	}

	now = now.Add(pruneInterval + time.Minute)
	p.write(context.Background(), recorded{
		instanceID: "inst-a",
		obs:        instance.PlayerObservation{TS: now, Players: intPtr(3)},
	})
	if got := historyLen(t, db); got != 2 {
		t.Fatalf("%d rows after the sweep, want the two inside the retention horizon", got)
	}
}

// TestAnUnstampedObservationTakesTheClock. A stream reset has no line behind it and so no
// Docker timestamp; the gap it records still needs a time.
func TestAnUnstampedObservationTakesTheClock(t *testing.T) {
	rt, db, fake, _, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	now := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)
	p := NewPlayerRecorder(db)
	p.now = func() time.Time { return now }
	p.lastPrune = now
	p.write(context.Background(), recorded{instanceID: "inst-a"})

	rows, err := db.ListPlayerObservations(t.Context(), "inst-a", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ObservedAt.Equal(now) {
		t.Fatalf("recorded %v, want one row stamped %s", rows, now)
	}
	if rows[0].Players != nil {
		t.Errorf("the gap recorded as %d, want null", *rows[0].Players)
	}
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
