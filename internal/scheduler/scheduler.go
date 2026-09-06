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
}

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
	next, err := Next(sc.Cron, now)
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
		if err := s.DB.MarkScheduleRun(ctx, sc.ID, now, next); err != nil {
			slog.ErrorContext(ctx, "scheduler could not initialise a schedule",
				slog.String("schedule_id", sc.ID), slog.Any("error", err))
		}
		return
	}

	if err := s.Enqueue(ctx, sc); err != nil {
		slog.ErrorContext(ctx, "scheduled job not enqueued",
			slog.String("schedule_id", sc.ID), slog.String("kind", sc.Kind), slog.Any("error", err))
	}
	if err := s.DB.MarkScheduleRun(ctx, sc.ID, now, next); err != nil {
		slog.ErrorContext(ctx, "scheduler could not advance a schedule",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
	}
}
