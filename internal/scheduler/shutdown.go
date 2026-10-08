package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// Lead times before a planned power cut: players are warned at ShutdownWarnLead, and every
// running server is stopped from ShutdownStopLead until the power goes off.
const (
	ShutdownWarnLead = 7 * time.Minute
	ShutdownStopLead = 2 * time.Minute
)

// Errors PowerOffTime returns for text it refuses.
var (
	ErrBadPowerOffTime  = errors.New("not a time the panel can read")
	ErrPastPowerOffTime = errors.New("that time has already passed")
)

// Shutdowns is the planned power cut clock. Before the soonest planned power cut it warns the
// players of every running server, then stops each running server, and keeps stopping any
// that runs again until the power goes off. It submits stops; it never runs one itself.
type Shutdowns struct {
	DB       *store.DB
	Interval time.Duration
	// Stop submits a stop of inst ahead of the power cut at powerOff.
	Stop func(ctx context.Context, inst *store.Instance, powerOff time.Time) error
	// Warn tells inst's players that the server stops within left, and reports false when the
	// line should be sent again on a later tick.
	Warn func(ctx context.Context, inst *store.Instance, left time.Duration) bool

	// warnedFor is the planned shutdown that warned holds instance ids for. Tick runs on one
	// goroutine, so neither needs a lock.
	warnedFor string
	warned    map[string]bool
}

// Run ticks until ctx is cancelled, once immediately so a daemon that boots inside a warning or
// stop window acts at once.
func (s *Shutdowns) Run(ctx context.Context) {
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

// Tick acts on the soonest planned power cut after now. A later one waits its turn, so two
// close together never warn the same players twice.
func (s *Shutdowns) Tick(ctx context.Context, now time.Time) {
	planned, err := s.DB.UpcomingShutdowns(ctx, now)
	if err != nil {
		slog.ErrorContext(ctx, "planned shutdowns could not be read", slog.Any("error", err))
		return
	}
	if len(planned) == 0 || now.Before(planned[0].PowerOffAt.Add(-ShutdownWarnLead)) {
		return
	}
	next := &planned[0]
	insts, err := s.DB.ListInstances(ctx, nil)
	if err != nil {
		slog.ErrorContext(ctx, "planned shutdown could not list servers", slog.Any("error", err))
		return
	}
	stopAt := next.PowerOffAt.Add(-ShutdownStopLead)
	for i := range insts {
		inst := &insts[i]
		if instance.State(inst.State) != instance.StateRunning || inst.ContainerID == nil {
			continue
		}
		if now.Before(stopAt) {
			s.warn(ctx, next.ID, inst, stopAt.Sub(now))
			continue
		}
		s.stop(ctx, next, inst)
	}
}

// warn warns inst's players once per planned shutdown.
func (s *Shutdowns) warn(ctx context.Context, shutdownID string, inst *store.Instance, left time.Duration) {
	if s.Warn == nil {
		return
	}
	if s.warnedFor != shutdownID {
		s.warnedFor, s.warned = shutdownID, map[string]bool{}
	}
	if !s.warned[inst.ID] && s.Warn(ctx, inst, left) {
		s.warned[inst.ID] = true
	}
}

// stop submits a stop of inst. A server busy with another job is retried on the next tick.
func (s *Shutdowns) stop(ctx context.Context, p *store.PlannedShutdown, inst *store.Instance) {
	err := s.Stop(ctx, inst, p.PowerOffAt)
	var conflict *store.JobConflict
	switch {
	case err == nil:
		slog.InfoContext(ctx, "planned shutdown stopping server",
			slog.String("instance_id", inst.ID), slog.Time("power_off_at", p.PowerOffAt))
	case errors.As(err, &conflict):
		slog.DebugContext(ctx, "planned shutdown waits for a running job",
			slog.String("instance_id", inst.ID), slog.String("job_id", conflict.JobID))
	default:
		slog.WarnContext(ctx, "planned shutdown could not stop server",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
	}
}

// PowerOffTime reads a typed power cut time in zone and returns it as a UTC instant after now.
// It accepts "15:04", the next such time of day, as well as "2006-01-02 15:04" and
// "2006-01-02T15:04".
func PowerOffTime(text, zone string, now time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" {
		return time.Time{}, fmt.Errorf("load time zone %q: %w", zone, ErrBadPowerOffTime)
	}
	text = strings.TrimSpace(text)
	var at time.Time
	if clock, err := time.Parse("15:04", text); err == nil {
		today := now.In(loc)
		at = time.Date(today.Year(), today.Month(), today.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
		if !at.After(now) {
			at = time.Date(today.Year(), today.Month(), today.Day()+1, clock.Hour(), clock.Minute(), 0, 0, loc)
		}
	} else if at, err = parseDateTime(text, loc); err != nil {
		return time.Time{}, err
	}
	if !at.After(now) {
		return time.Time{}, ErrPastPowerOffTime
	}
	return at.UTC(), nil
}

func parseDateTime(text string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if at, err := time.ParseInLocation(layout, text, loc); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: %w", text, ErrBadPowerOffTime)
}
