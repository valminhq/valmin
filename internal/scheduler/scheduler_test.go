package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// open returns a migrated database in a temp directory.
func open(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// seedSchedule writes a global schedule due at nextRunAt. Every case here uses the same
// nightly expression; what varies is when the clock next looks.
func seedSchedule(t *testing.T, db *store.DB, nextRunAt *time.Time) *store.Schedule {
	t.Helper()
	s := &store.Schedule{
		ID: store.NewID(), Kind: "prune", Cron: "0 3 * * *", Payload: "{}",
		Enabled: true, NextRunAt: nextRunAt,
	}
	if err := db.CreateSchedule(t.Context(), s); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return s
}

func reread(t *testing.T, db *store.DB, id string) *store.Schedule {
	t.Helper()
	s, err := db.ScheduleByID(t.Context(), id)
	if err != nil || s == nil {
		t.Fatalf("ScheduleByID(%s) = %v, %v", id, s, err)
	}
	return s
}

// counting is an Enqueuer that records what it was handed.
func counting(fired *[]string) Enqueuer {
	return func(_ context.Context, s *store.Schedule) error {
		*fired = append(*fired, s.ID)
		return nil
	}
}

func at(t *testing.T, stamp string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}

// Asserts an expression the parser cannot read is an error rather than a schedule that never
// fires, so POST /schedules can refuse it at the moment it is written.
func TestNextRefusesAnExpressionItCannotRead(t *testing.T) {
	for _, expr := range []string{"", "not a cron", "* * * *", "99 * * * *", "@sometimes"} {
		if _, err := Next(expr, time.Now()); !errors.Is(err, ErrBadCron) {
			t.Errorf("Next(%q) = %v, want ErrBadCron", expr, err)
		}
	}
}

// Asserts the five-field form and the shorthands the panel offers both parse to the next time
// after the one given, never to it.
func TestNextIsStrictlyAfterTheTimeGiven(t *testing.T) {
	tests := []struct {
		expr, from, want string
	}{
		{"0 3 * * *", "2026-09-06T02:00:00Z", "2026-09-06T03:00:00Z"},
		{"0 3 * * *", "2026-09-06T03:00:00Z", "2026-09-07T03:00:00Z"},
		{"@daily", "2026-09-06T12:00:00Z", "2026-09-07T00:00:00Z"},
		{"*/15 * * * *", "2026-09-06T03:01:00Z", "2026-09-06T03:15:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.expr+" from "+tt.from, func(t *testing.T) {
			got, err := Next(tt.expr, at(t, tt.from))
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if !got.Equal(at(t, tt.want)) {
				t.Errorf("Next = %s, want %s", got.Format(time.RFC3339), tt.want)
			}
		})
	}
}

// Asserts a schedule whose time has passed fires once and is moved past the tick.
func TestTickFiresADueScheduleAndAdvancesIt(t *testing.T) {
	db := open(t)
	now := at(t, "2026-09-06T03:00:30Z")
	due := now.Add(-time.Minute)
	sc := seedSchedule(t, db, &due)

	var fired []string
	(&Scheduler{DB: db, Enqueue: counting(&fired)}).Tick(t.Context(), now)

	if len(fired) != 1 || fired[0] != sc.ID {
		t.Fatalf("fired %v, want exactly %s", fired, sc.ID)
	}
	after := reread(t, db, sc.ID)
	if after.LastRunAt == nil || !after.LastRunAt.Equal(now) {
		t.Errorf("last_run_at = %v, want %s", after.LastRunAt, now.Format(time.RFC3339))
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(at(t, "2026-09-07T03:00:00Z")) {
		t.Errorf("next_run_at = %v, want tomorrow's 03:00", after.NextRunAt)
	}
}

// Asserts a daemon that was down across three due times produces one run, not three: the next
// time is computed from now rather than from the stale one it missed (12 §11).
func TestTickDoesNotBackfillMissedRuns(t *testing.T) {
	db := open(t)
	missed := at(t, "2026-09-03T03:00:00Z")
	sc := seedSchedule(t, db, &missed)
	now := at(t, "2026-09-06T09:15:00Z")

	var fired []string
	s := &Scheduler{DB: db, Enqueue: counting(&fired)}
	s.Tick(t.Context(), now)

	if len(fired) != 1 {
		t.Fatalf("fired %d times for three missed occurrences, want 1", len(fired))
	}
	if got := reread(t, db, sc.ID).NextRunAt; got == nil || !got.Equal(at(t, "2026-09-07T03:00:00Z")) {
		t.Fatalf("next_run_at = %v, want the next 03:00 after now", got)
	}
	// And the tick right after it finds nothing: the schedule is genuinely past.
	s.Tick(t.Context(), now.Add(time.Minute))
	if len(fired) != 1 {
		t.Errorf("fired %d times over two ticks, want 1", len(fired))
	}
}

// Asserts a schedule with no next_run_at is given one without firing: it was due only for
// having no time set, which says nothing about whether its expression matched.
func TestTickInitialisesAScheduleWithoutFiringIt(t *testing.T) {
	db := open(t)
	sc := seedSchedule(t, db, nil)
	now := at(t, "2026-09-06T09:15:00Z")

	var fired []string
	(&Scheduler{DB: db, Enqueue: counting(&fired)}).Tick(t.Context(), now)

	if len(fired) != 0 {
		t.Errorf("fired %v for a schedule that had never been timed", fired)
	}
	if got := reread(t, db, sc.ID).NextRunAt; got == nil || !got.Equal(at(t, "2026-09-07T03:00:00Z")) {
		t.Errorf("next_run_at = %v, want the next 03:00", got)
	}
}

// Asserts a disabled schedule is not due however long it has been waiting.
func TestTickIgnoresADisabledSchedule(t *testing.T) {
	db := open(t)
	past := at(t, "2026-09-01T03:00:00Z")
	sc := seedSchedule(t, db, &past)
	sc.Enabled = false
	if err := db.UpdateSchedule(t.Context(), sc); err != nil {
		t.Fatal(err)
	}

	var fired []string
	(&Scheduler{DB: db, Enqueue: counting(&fired)}).Tick(t.Context(), at(t, "2026-09-06T09:15:00Z"))

	if len(fired) != 0 {
		t.Errorf("fired %v for a disabled schedule", fired)
	}
}

// Asserts a row whose expression was edited outside the panel is left alone rather than given a
// guessed time: advancing it would hide a schedule that is silently never running.
func TestTickLeavesAnUnreadableExpressionAlone(t *testing.T) {
	db := open(t)
	due := at(t, "2026-09-06T03:00:00Z")
	sc := seedSchedule(t, db, &due)
	if _, err := db.Writer.ExecContext(t.Context(),
		`UPDATE scheduled_jobs SET cron = 'nonsense' WHERE id = ?`, sc.ID); err != nil {
		t.Fatal(err)
	}

	var fired []string
	(&Scheduler{DB: db, Enqueue: counting(&fired)}).Tick(t.Context(), at(t, "2026-09-06T03:00:30Z"))

	if len(fired) != 0 {
		t.Errorf("fired %v for an expression the panel cannot read", fired)
	}
	if got := reread(t, db, sc.ID).LastRunAt; got != nil {
		t.Errorf("last_run_at = %v, want it untouched", got)
	}
}

// Asserts a schedule whose enqueuer failed is still advanced: one that is not moves back to due
// on the next tick, and every tick after that.
func TestTickAdvancesEvenWhenTheEnqueuerFails(t *testing.T) {
	db := open(t)
	due := at(t, "2026-09-06T03:00:00Z")
	sc := seedSchedule(t, db, &due)
	now := at(t, "2026-09-06T03:00:30Z")

	s := &Scheduler{DB: db, Enqueue: func(context.Context, *store.Schedule) error {
		return errors.New("nothing could be submitted")
	}}
	s.Tick(t.Context(), now)

	if got := reread(t, db, sc.ID).NextRunAt; got == nil || !got.Equal(at(t, "2026-09-07T03:00:00Z")) {
		t.Errorf("next_run_at = %v, want the schedule moved past a tick it could not run", got)
	}
}
