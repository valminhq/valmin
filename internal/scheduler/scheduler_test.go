package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
		Enabled: true, NextRunAt: nextRunAt, UnknownPlayers: store.UnknownPlayersWait,
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
	if err := db.UpdateSchedule(t.Context(), sc, false); err != nil {
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

// TestParsesWhatTheScheduleBuilderEmits pins the contract between the panel's schedule builder
// and this parser.
//
// The builder writes expressions rather than reading them, so nothing in the SPA can tell it
// that a form it emits is one ParseStandard refuses — the operator would find out from a failed
// create. The forms are few and fixed, so they are listed here instead: a time input yields a
// zero-padded hour, which is the one that looks like it might not survive an integer field.
func TestParsesWhatTheScheduleBuilderEmits(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) // a Monday

	cases := []struct {
		name, expr, next string
	}{
		{"every hour", "0 */1 * * *", "Mon 13:00"},
		{"every two hours", "0 */2 * * *", "Mon 14:00"},
		{"every three hours", "0 */3 * * *", "Mon 15:00"},
		{"every four hours", "0 */4 * * *", "Mon 16:00"},
		{"every six hours", "0 */6 * * *", "Mon 18:00"},
		{"every eight hours", "0 */8 * * *", "Mon 16:00"},
		{"every twelve hours", "0 */12 * * *", "Tue 00:00"},
		{"daily, padded hour", "00 04 * * *", "Tue 04:00"},
		{"daily, padded midnight", "05 00 * * *", "Tue 00:05"},
		{"daily, unpadded", "30 21 * * *", "Mon 21:30"},
		{"weekly on Sunday", "00 04 * * 0", "Sun 04:00"},
		{"weekly on Saturday", "45 23 * * 6", "Sat 23:45"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Next(tc.expr, now)
			if err != nil {
				t.Fatalf("Next(%q) = %v, want it accepted", tc.expr, err)
			}
			if fired := got.Format("Mon 15:04"); fired != tc.next {
				t.Errorf("Next(%q) = %s, want %s", tc.expr, fired, tc.next)
			}
		})
	}
}

// Every "every N hours" choice the builder offers must divide the day, or the last run of one
// day and the first of the next are closer together than the label claims.
func TestTheHourlyChoicesDivideTheDay(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 2, 3, 4, 6, 8, 12} {
		expr := fmt.Sprintf("0 */%d * * *", n)
		at := now
		for range 24 / n {
			next, err := Next(expr, at)
			if err != nil {
				t.Fatalf("Next(%q): %v", expr, err)
			}
			if gap := next.Sub(at); gap != time.Duration(n)*time.Hour {
				t.Errorf("%s: gap after %s was %s, want %dh", expr, at.Format("15:04"), gap, n)
			}
			at = next
		}
		if at != now.Add(24*time.Hour) {
			t.Errorf("%s: %d runs landed on %s, not exactly one day later", expr, 24/n, at)
		}
	}
}

// seedPolicy writes a schedule due at due that waits up to maxDeferral for players to leave.
func seedPolicy(t *testing.T, db *store.DB, due time.Time, maxDeferral time.Duration) *store.Schedule {
	t.Helper()
	s := &store.Schedule{
		ID: store.NewID(), Kind: "restart", Cron: "0 3 * * *", Payload: "{}", Enabled: true,
		NextRunAt: &due, WaitForEmpty: true, MaxDeferral: maxDeferral,
		UnknownPlayers: store.UnknownPlayersWait,
	}
	if err := db.CreateSchedule(t.Context(), s); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return s
}

// Asserts the whole life of a held run: an occupied server holds it without advancing it, a
// second occupied tick changes nothing, and the tick after the server empties runs it.
func TestTickHoldsARunUntilTheServerEmpties(t *testing.T) {
	db := open(t)
	due := at(t, "2026-09-06T03:00:00Z")
	sc := seedPolicy(t, db, due, 2*time.Hour)
	first := at(t, "2026-09-06T03:00:30Z")

	var fired []string
	held, occupied := 0, true
	s := &Scheduler{
		DB: db, Enqueue: counting(&fired),
		Occupied: func(context.Context, *store.Schedule) bool { return occupied },
		Held:     func(*store.Schedule) { held++ },
	}

	s.Tick(t.Context(), first)
	after := reread(t, db, sc.ID)
	if len(fired) != 0 || held != 1 {
		t.Fatalf("fired %v, held %d times; want nothing fired and one hold", fired, held)
	}
	if after.DeferredSince == nil || !after.DeferredSince.Equal(first) {
		t.Errorf("deferred_since = %v, want %s", after.DeferredSince, first.Format(time.RFC3339))
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(due) {
		t.Errorf("next_run_at = %v, want it kept at %s", after.NextRunAt, due.Format(time.RFC3339))
	}

	s.Tick(t.Context(), first.Add(time.Minute))
	if again := reread(t, db, sc.ID); len(fired) != 0 || held != 1 || !again.DeferredSince.Equal(first) {
		t.Fatalf("second occupied tick: fired %v, held %d, deferred_since %v; want no change",
			fired, held, again.DeferredSince)
	}

	occupied = false
	s.Tick(t.Context(), first.Add(2*time.Minute))
	after = reread(t, db, sc.ID)
	if len(fired) != 1 || held != 2 {
		t.Fatalf("emptied server: fired %v, held %d; want one run and the hold ended", fired, held)
	}
	if after.DeferredSince != nil {
		t.Errorf("deferred_since = %v, want it cleared", after.DeferredSince)
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(at(t, "2026-09-07T03:00:00Z")) {
		t.Errorf("next_run_at = %v, want tomorrow's 03:00", after.NextRunAt)
	}
}

// Asserts a run held for its maximum deferral goes ahead with players still connected.
func TestTickRunsAHeldRunAtItsMaximumDeferral(t *testing.T) {
	db := open(t)
	due := at(t, "2026-09-06T03:00:00Z")
	sc := seedPolicy(t, db, due, 15*time.Minute)
	if err := db.DeferSchedule(t.Context(), sc.ID, due); err != nil {
		t.Fatal(err)
	}

	var fired []string
	s := &Scheduler{
		DB: db, Enqueue: counting(&fired),
		Occupied: func(context.Context, *store.Schedule) bool { return true },
	}
	s.Tick(t.Context(), due.Add(14*time.Minute))
	if len(fired) != 0 {
		t.Fatalf("fired %v before the maximum deferral", fired)
	}
	s.Tick(t.Context(), due.Add(15*time.Minute))
	if len(fired) != 1 {
		t.Fatalf("fired %d times at the maximum deferral, want 1", len(fired))
	}
	if got := reread(t, db, sc.ID).DeferredSince; got != nil {
		t.Errorf("deferred_since = %v, want it cleared", got)
	}
}

// Asserts players are warned when a hold starts and once more per deadline when FinalWarning or
// less remains, that a hold no longer than FinalWarning is warned only when it starts, that a
// changed maximum deferral warns for the new deadline, and that a failed final warning is retried.
func TestTickWarnsPlayersOfAHeldRun(t *testing.T) {
	tests := []struct {
		name        string
		maxDeferral time.Duration
		// edits sets the schedule's maximum deferral before the tick at that offset.
		edits    map[time.Duration]time.Duration
		failures int
		ticks    []time.Duration
		want     []time.Duration
	}{
		{
			name:        "long hold",
			maxDeferral: 30 * time.Minute,
			ticks: []time.Duration{
				0,
				10 * time.Minute,
				24 * time.Minute,
				25*time.Minute + 30*time.Second,
				27 * time.Minute,
			},
			want: []time.Duration{30 * time.Minute, 4*time.Minute + 30*time.Second},
		},
		{
			name:        "short hold",
			maxDeferral: FinalWarning,
			ticks:       []time.Duration{0, time.Minute, 4 * time.Minute},
			want:        []time.Duration{FinalWarning},
		},
		{
			name:        "shortened below the final warning",
			maxDeferral: 2 * time.Hour,
			edits:       map[time.Duration]time.Duration{time.Minute: 4 * time.Minute},
			ticks:       []time.Duration{0, time.Minute, 2 * time.Minute},
			want:        []time.Duration{2 * time.Hour, 3 * time.Minute},
		},
		{
			name:        "extended after the final warning",
			maxDeferral: 30 * time.Minute,
			edits:       map[time.Duration]time.Duration{27 * time.Minute: 3 * time.Hour},
			ticks:       []time.Duration{0, 26 * time.Minute, 27 * time.Minute, 176 * time.Minute, 177 * time.Minute},
			want:        []time.Duration{30 * time.Minute, 4 * time.Minute, 4 * time.Minute},
		},
		{
			name:        "failed final warning",
			maxDeferral: 30 * time.Minute,
			failures:    1,
			ticks:       []time.Duration{0, 25 * time.Minute, 26 * time.Minute, 27 * time.Minute},
			want:        []time.Duration{30 * time.Minute, 5 * time.Minute, 4 * time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := open(t)
			due := at(t, "2026-09-06T03:00:00Z")
			sc := seedPolicy(t, db, due, tt.maxDeferral)

			var fired []string
			var warned []time.Duration
			failed := 0
			s := &Scheduler{
				DB: db, Enqueue: counting(&fired),
				Occupied: func(context.Context, *store.Schedule) bool { return true },
				Warn: func(_ context.Context, _ *store.Schedule, left time.Duration) bool {
					warned = append(warned, left)
					if left <= FinalWarning && failed < tt.failures {
						failed++
						return false
					}
					return true
				},
			}
			for _, d := range tt.ticks {
				if maxDeferral, ok := tt.edits[d]; ok {
					row := reread(t, db, sc.ID)
					row.MaxDeferral = maxDeferral
					if err := db.UpdateSchedule(t.Context(), row, false); err != nil {
						t.Fatalf("UpdateSchedule: %v", err)
					}
				}
				s.Tick(t.Context(), due.Add(d))
			}
			if len(fired) != 0 {
				t.Fatalf("fired %v before the maximum deferral", fired)
			}
			if !slices.Equal(warned, tt.want) {
				t.Errorf("warned with %v left, want %v", warned, tt.want)
			}
		})
	}
}

// Asserts a schedule without wait_for_empty runs on time and never asks whether players are
// connected.
func TestTickDoesNotConsultOccupiedWithoutThePolicy(t *testing.T) {
	db := open(t)
	due := at(t, "2026-09-06T03:00:00Z")
	seedSchedule(t, db, &due)

	var fired []string
	asked := 0
	s := &Scheduler{
		DB: db, Enqueue: counting(&fired),
		Occupied: func(context.Context, *store.Schedule) bool { asked++; return true },
	}
	s.Tick(t.Context(), due.Add(time.Minute))
	if len(fired) != 1 || asked != 0 {
		t.Errorf("fired %v, Occupied asked %d times; want one run and no question", fired, asked)
	}
}
