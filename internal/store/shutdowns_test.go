package store

import (
	"testing"
	"time"
)

// TestPlannedShutdowns asserts upcoming entries list soonest first, a new entry prunes the past
// ones, and a delete reports whether the entry existed.
func TestPlannedShutdowns(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	exec(t, db.Writer, `INSERT INTO planned_shutdowns (id, power_off_at, created_at) VALUES ('past', ?, ?)`,
		FormatTime(now.Add(-time.Hour)), FormatTime(now))
	for _, s := range []PlannedShutdown{
		{ID: "later", PowerOffAt: now.Add(2 * time.Hour), CreatedByName: "Discord: den (1)"},
		{ID: "sooner", PowerOffAt: now.Add(time.Hour)},
	} {
		if err := db.CreatePlannedShutdown(ctx, &s, nil); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.UpcomingShutdowns(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "sooner" || got[1].ID != "later" ||
		!got[0].PowerOffAt.Equal(now.Add(time.Hour)) || got[1].CreatedByName != "Discord: den (1)" {
		t.Fatalf("upcoming = %+v", got)
	}
	var past int
	if err := db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM planned_shutdowns WHERE id = 'past'`).
		Scan(&past); err != nil || past != 0 {
		t.Fatalf("past rows = %d, %v; want pruned", past, err)
	}

	for _, tt := range []struct {
		id   string
		want bool
	}{{"sooner", true}, {"sooner", false}} {
		found, err := db.DeletePlannedShutdown(ctx, tt.id, nil)
		if err != nil || found != tt.want {
			t.Fatalf("DeletePlannedShutdown(%q) = %v, %v; want %v", tt.id, found, err, tt.want)
		}
	}
}
