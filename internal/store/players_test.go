package store

import (
	"testing"
	"time"
)

func at(mins int) time.Time {
	return time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC).Add(time.Duration(mins) * time.Minute)
}

func ptr(n int) *int { return &n }

// TestObservationGapsRoundTrip. A null count is the point of the table: it says the panel
// stopped being able to tell, and a round trip that turned it into a zero would report an
// empty server instead.
func TestObservationGapsRoundTrip(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, "a", 2456)

	for i, players := range []*int{ptr(0), ptr(1), nil, ptr(2)} {
		if err := db.RecordPlayerObservation(t.Context(), id, at(i), players); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.ListPlayerObservations(t.Context(), id, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("read back %d observations, want 4", len(rows))
	}
	// Newest first.
	if rows[0].Players == nil || *rows[0].Players != 2 {
		t.Errorf("newest = %v, want 2", rows[0].Players)
	}
	if rows[1].Players != nil {
		t.Errorf("the gap read back as %d, want null", *rows[1].Players)
	}
	if !rows[3].ObservedAt.Equal(at(0)) {
		t.Errorf("oldest observed_at = %s, want %s", rows[3].ObservedAt, at(0))
	}
}

// TestPagingIsStableAcrossEqualTimestamps. Docker stamps to the second, and the count can
// change twice inside one: a cursor on the timestamp alone would either repeat or skip a row
// at the boundary.
func TestPagingIsStableAcrossEqualTimestamps(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, "a", 2456)
	for i := range 6 {
		if err := db.RecordPlayerObservation(t.Context(), id, at(0), ptr(i)); err != nil {
			t.Fatal(err)
		}
	}

	var (
		seen           = map[string]bool{}
		beforeAt, byID string
	)
	for range 6 {
		page, err := db.ListPlayerObservations(t.Context(), id, beforeAt, byID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			if seen[row.ID] {
				t.Fatalf("observation %s came back on two pages", row.ID)
			}
			seen[row.ID] = true
		}
		last := page[len(page)-1]
		beforeAt, byID = FormatTime(last.ObservedAt), last.ID
	}
	if len(seen) != 6 {
		t.Fatalf("paged over %d of 6 observations", len(seen))
	}
}

// TestRetentionBoundsHistoryAndNothingElse. The sweep runs on a fixed clock, and the audit
// trail is permanent (12 §7) — it is a different table and must still be there afterwards.
func TestRetentionBoundsHistoryAndNothingElse(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, "a", 2456)
	user := seedUser(t, db, "u")
	exec(t, db.Writer,
		`INSERT INTO audit_log (id, user_id, instance_id, action, created_at) VALUES (?, ?, ?, ?, ?)`,
		"aud-1", user, id, "instance.start", FormatTime(at(-100000)))

	old, recent := at(0), at(60*24*40)
	for _, when := range []time.Time{old, recent} {
		if err := db.RecordPlayerObservation(t.Context(), id, when, ptr(1)); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.PrunePlayerObservations(t.Context(), recent.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d observations, want 1", n)
	}
	rows, err := db.ListPlayerObservations(t.Context(), id, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].ObservedAt.Equal(recent) {
		t.Fatalf("retention kept %d rows, want only the recent one", len(rows))
	}

	var audits int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT count(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("the audit trail lost rows to the history sweep: %d remain", audits)
	}
}

// TestDeletingAnInstanceTakesItsHistory. The rows are per instance and meaningless without
// it; the cascade is the schema's, and 10 §4.3's foreign_keys pragma is what makes it fire.
func TestDeletingAnInstanceTakesItsHistory(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, "a", 2456)
	if err := db.RecordPlayerObservation(t.Context(), id, at(0), ptr(1)); err != nil {
		t.Fatal(err)
	}
	exec(t, db.Writer, `DELETE FROM instances WHERE id = ?`, id)

	rows, err := db.ListPlayerObservations(t.Context(), id, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d observations outlived their instance", len(rows))
	}
}
