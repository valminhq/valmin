// Package scheduler is the panel's clock. It reads scheduled_jobs, decides what is due, and
// hands each due row to an enqueuer — it never executes a job itself (12 §11).
//
// Specification: 12 §11, 02 §2.6, ADR-030.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/valminhq/valmin/internal/store"
)

// ErrBadCron is a cron expression the parser refused. It reaches an operator as a field error
// on POST /schedules, so an unparseable expression is never stored and never discovered at
// three in the morning.
var ErrBadCron = errors.New("not a valid cron expression")

// Next reports when expr next fires strictly after t, in t's own location.
//
// robfig/cron's ParseStandard is the five-field POSIX form plus its @-shorthands, and only its
// parser is used: cron.Cron's own runner keeps its own goroutines and its own idea of what is
// running, which would be a second job engine beside the one 12 specifies.
func Next(expr string, t time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadCron, err)
	}
	next := sched.Next(t)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %q never fires", ErrBadCron, expr)
	}
	return next, nil
}

// NextIn returns the next occurrence on a schedule's wall clock as a UTC instant.
func NextIn(expr, zone string, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load schedule timezone %q: %w", zone, err)
	}
	next, err := Next(expr, after.In(loc))
	if err != nil {
		return time.Time{}, err
	}
	return next.UTC(), nil
}

// NextRun is the next time a schedule fires after after, on its own wall clock, with the two
// daylight-saving edges settled. last is the run that just fired, or nil.
//
// When the clocks go back, a time in the repeated hour reads the same twice; the second is not
// a new occurrence and is skipped. When they go forward, a time in the missing hour never
// happens; the run fires once at the instant the wall clock jumps past it instead of being
// skipped for the day.
func NextRun(expr, zone string, after time.Time, last *time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load schedule timezone %q: %w", zone, err)
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrBadCron, err)
	}
	next := sched.Next(after.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %q never fires", ErrBadCron, expr)
	}
	if last != nil && sameWallClock(next, last.In(loc)) {
		next = sched.Next(next)
	}
	// The same expression on the offset in force at after finds an occurrence the jump erased.
	_, offset := after.In(loc).Zone()
	fixed := time.FixedZone("", offset)
	if early := sched.Next(after.In(fixed)); !early.IsZero() && early.Before(next) {
		y, mo, d := early.Date()
		inLoc := time.Date(y, mo, d, early.Hour(), early.Minute(), 0, 0, loc)
		if (inLoc.Hour() != early.Hour() || inLoc.Minute() != early.Minute()) && early.After(after) {
			// The wall clock never showed early's time. The instant it names at the old
			// offset is just past the jump, which is when the run happens.
			next = early
		}
	}
	return next.UTC(), nil
}

// sameWallClock reports whether a and b, both in one zone, show the same date, hour and minute.
func sameWallClock(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

// Interval estimates expr's period as the gap between its next two fires after from. An
// irregular expression yields an approximation, which is all a staleness threshold needs.
func Interval(expr string, from time.Time) (time.Duration, error) {
	first, err := Next(expr, from)
	if err != nil {
		return 0, err
	}
	second, err := Next(expr, first)
	if err != nil {
		return 0, err
	}
	return second.Sub(first), nil
}

// Enqueuer submits the job a due schedule asks for. It reports an error only when the tick
// could not be resolved at all; a lock already held is a skip the enqueuer records itself, and
// is not an error here (ADR-030).
type Enqueuer func(ctx context.Context, s *store.Schedule) error

// Scheduler is the tick loop. Interval is how often it asks the database what is due, not how
// precise a schedule is: a cron expression naming 03:00 fires on the first tick after 03:00.
type Scheduler struct {
	DB       *store.DB
	Interval time.Duration
	Enqueue  Enqueuer
	// Occupied reports whether players may be connected to the server a due run would stop.
	// Nil holds nothing.
	Occupied func(ctx context.Context, s *store.Schedule) bool
	// Held is called after a schedule starts or stops holding a due run. Optional.
	Held func(s *store.Schedule)
	// Warn tells the players that a held run stops their server within left at the latest, and
	// reports false when the line should be sent again. It is called when a hold starts and
	// once more per deadline when FinalWarning or less remains. Optional.
	Warn func(ctx context.Context, s *store.Schedule, left time.Duration) bool

	// warned maps a schedule id to the hold deadline its final warning covers. Tick runs on one
	// goroutine, so it needs no lock.
	warned map[string]time.Time
}

// FinalWarning is how long before a held run's maximum deferral its players are warned again.
const FinalWarning = 5 * time.Minute

// Run ticks until ctx is cancelled. It ticks once immediately, so a daemon that starts after a
// due time does not wait a whole interval to notice.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Interval <= 0 {
		return
	}
	s.Tick(ctx, time.Now().UTC())

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx, time.Now().UTC())
		}
	}
}

// Tick enqueues everything due at now and advances each row past it. Exported so a test can
// drive the clock rather than wait for one.
//
// A schedule whose time passed while the daemon was down fires once, not once per missed
// occurrence: the next time is computed from now rather than from the stale next_run_at, so
// three missed 03:00s collapse into the one run that is still worth doing (12 §11).
func (s *Scheduler) Tick(ctx context.Context, now time.Time) {
	due, err := s.DB.DueSchedules(ctx, now)
	if err != nil {
		slog.ErrorContext(ctx, "scheduler could not read what is due", slog.Any("error", err))
		return
	}
	for i := range due {
		s.fire(ctx, &due[i], now)
	}
}

// fire runs one due schedule. The row is advanced whatever the enqueuer did: a schedule that
// could not run this time still has to move past this tick, or it fires again on the next one
// and every one after it.
func (s *Scheduler) fire(ctx context.Context, sc *store.Schedule, now time.Time) {
	next, err := NextRun(sc.Cron, sc.Timezone, now, sc.NextRunAt)
	if err != nil {
		// Stored expressions are validated on write, so this is a row edited outside the
		// panel. Left enabled and not advanced, because guessing a time for it would hide it.
		slog.ErrorContext(ctx, "schedule has an expression the panel cannot read",
			slog.String("schedule_id", sc.ID), slog.String("cron", sc.Cron), slog.Any("error", err))
		return
	}

	// A row that has never run gets its next time written without firing: it was due only
	// because it had no next_run_at, which says nothing about whether its expression matched.
	if sc.NextRunAt == nil {
		if err := s.DB.MarkScheduleRun(ctx, sc.ID, sc.NextRunAt, now, next); err != nil {
			slog.ErrorContext(ctx, "scheduler could not initialise a schedule",
				slog.String("schedule_id", sc.ID), slog.Any("error", err))
		}
		return
	}

	// A held row keeps its past next_run_at, so it is due again on the next tick.
	if s.hold(ctx, sc, now) {
		return
	}

	if !s.stillDue(ctx, sc) {
		return
	}
	if err := s.Enqueue(ctx, sc); err != nil {
		slog.ErrorContext(ctx, "scheduled job not enqueued",
			slog.String("schedule_id", sc.ID), slog.String("kind", sc.Kind), slog.Any("error", err))
	}
	if err := s.DB.MarkScheduleRun(ctx, sc.ID, sc.NextRunAt, now, next); err != nil {
		slog.ErrorContext(ctx, "scheduler could not advance a schedule",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return
	}
	if sc.DeferredSince != nil {
		delete(s.warned, sc.ID)
		s.held(sc)
	}
}

// stillDue re-reads a due row just before it fires, because the tick's copy may be stale: a
// schedule disabled or edited since then must not run on the old copy.
func (s *Scheduler) stillDue(ctx context.Context, sc *store.Schedule) bool {
	fresh, err := s.DB.ScheduleByID(ctx, sc.ID)
	if err != nil {
		slog.ErrorContext(ctx, "scheduler could not re-read a due schedule",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return false
	}
	if fresh == nil || !fresh.Enabled || fresh.Cron != sc.Cron || fresh.Timezone != sc.Timezone ||
		!sameInstant(fresh.NextRunAt, sc.NextRunAt) {
		slog.InfoContext(ctx, "schedule changed while it was due; not running the old copy",
			slog.String("schedule_id", sc.ID))
		return false
	}
	return true
}

// sameInstant reports whether two optional times are both absent or the same instant.
func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// hold reports whether a due run waits for players to leave, and records when the wait began.
// A run held for its schedule's maximum deferral goes ahead with players connected.
func (s *Scheduler) hold(ctx context.Context, sc *store.Schedule, now time.Time) bool {
	if !sc.WaitForEmpty || s.Occupied == nil {
		return false
	}
	if sc.DeferredSince != nil && !now.Before(sc.DeferredSince.Add(sc.MaxDeferral)) {
		slog.InfoContext(ctx, "scheduled run reached its maximum deferral",
			slog.String("schedule_id", sc.ID), slog.String("kind", sc.Kind))
		return false
	}
	if !s.Occupied(ctx, sc) {
		return false
	}
	if sc.DeferredSince != nil {
		s.finalWarning(ctx, sc, now)
		return true
	}
	if err := s.DB.DeferSchedule(ctx, sc.ID, now); err != nil {
		// Still held: the stamp is retried on the next tick.
		slog.ErrorContext(ctx, "scheduler could not record a held run",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return true
	}
	sc.DeferredSince = &now
	s.held(sc)
	s.warn(ctx, sc, sc.MaxDeferral)
	if sc.MaxDeferral <= FinalWarning {
		s.markWarned(ctx, sc.ID, now.Add(sc.MaxDeferral))
	}
	return true
}

// finalWarning warns once per deadline when FinalWarning or less remains, so an edit that moves
// the deadline warns again. A failed warning is retried on the next tick.
func (s *Scheduler) finalWarning(ctx context.Context, sc *store.Schedule, now time.Time) {
	deadline := sc.DeferredSince.Add(sc.MaxDeferral)
	left := deadline.Sub(now)
	if left > FinalWarning || s.warnedFor(ctx, sc.ID).Equal(deadline) {
		return
	}
	if s.warn(ctx, sc, left) {
		s.markWarned(ctx, sc.ID, deadline)
	}
}

// warnedKey is the kv row that keeps a schedule's last final warning across a restart.
func warnedKey(id string) string { return "schedule_final_warning:" + id }

// warnedFor is the hold deadline schedule id's final warning was last sent for, read from kv
// the first time after a restart.
func (s *Scheduler) warnedFor(ctx context.Context, id string) time.Time {
	if at, ok := s.warned[id]; ok {
		return at
	}
	var at time.Time
	if _, err := s.DB.KVGet(ctx, warnedKey(id), &at); err != nil {
		slog.WarnContext(ctx, "read a schedule's last warning", slog.String("schedule_id", id), slog.Any("error", err))
	}
	if s.warned == nil {
		s.warned = map[string]time.Time{}
	}
	s.warned[id] = at
	return at
}

// markWarned records that the final warning for the hold ending at deadline needs no resend,
// here and in kv, so a restart inside the warning window does not warn again.
func (s *Scheduler) markWarned(ctx context.Context, id string, deadline time.Time) {
	if s.warned == nil {
		s.warned = map[string]time.Time{}
	}
	s.warned[id] = deadline
	if err := s.DB.KVSet(ctx, warnedKey(id), deadline); err != nil {
		slog.WarnContext(ctx, "store a schedule's last warning", slog.String("schedule_id", id), slog.Any("error", err))
	}
}

// warn calls the Warn hook, if one is set, and reports whether it needs no retry.
func (s *Scheduler) warn(ctx context.Context, sc *store.Schedule, left time.Duration) bool {
	return s.Warn == nil || s.Warn(ctx, sc, left)
}

// held calls the Held hook, if one is set.
func (s *Scheduler) held(sc *store.Schedule) {
	if s.Held != nil {
		s.Held(sc)
	}
}
