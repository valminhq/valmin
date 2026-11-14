package control

import (
	"context"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// The watchdog's limits. The game writes a status line every ten minutes from its main thread,
// so a running server that stays quiet for fifteen has a stopped main thread. A server that has
// logged none this boot gets longer: its first line comes ten minutes after the world loads, and
// the first start after a game update can spend minutes converting the world before that.
const (
	hangAfterStatus = 15 * time.Minute
	hangAfterStart  = 25 * time.Minute
	restartCooldown = time.Hour
	// suspendGap is the gap between two passes that means the host or the daemon was suspended or
	// stalled, not that the server went quiet. A live server is never quiet for more than ten of
	// the fifteen minutes allowed, so a shorter gap cannot make one look hung.
	suspendGap = 2 * time.Minute
)

// hangWatch is the watchdog's memory: when it last ran, what it has already reported, which
// restarts it still owes and when it last restarted each server. It is not saved, so a daemon
// restart can repeat one report. The zero value is ready to use, and one goroutine owns it.
//
// It works in wall-clock time throughout. Go's monotonic clock stops while the host is suspended,
// and the log timestamps a silence is measured from carry no monotonic reading, so every time it
// takes is stripped of one first.
type hangWatch struct {
	lastTick time.Time
	// rearmed is the pass that began or ended a gap in the watchdog's passes, a suspend or a
	// daemon start. No silence is counted from before it, because the log may not have been read
	// up to date.
	rearmed time.Time
	// reported and owed hold, per server, the silence that began at the instant stored: reported
	// once it has been announced, owed while a restart it asked for is still to be made.
	reported  map[string]time.Time
	owed      map[string]time.Time
	restarted map[string]time.Time
	noted     map[string]time.Time
}

// tick records a pass at now. The first pass, and any pass after a gap that means the host or the
// daemon was suspended, restarts every server's silence from now. It returns the gap when that is
// what happened, and zero otherwise.
func (h *hangWatch) tick(now time.Time) time.Duration {
	now = now.Round(0)
	var gap time.Duration
	if !h.lastTick.IsZero() && now.Sub(h.lastTick) > suspendGap {
		gap = now.Sub(h.lastTick)
	}
	if h.lastTick.IsZero() || gap > 0 {
		h.rearmed = now
	}
	h.lastTick = now
	return gap
}

// silent reports whether a running server needs the watchdog's attention, and returns the instant
// its silence began, which identifies the episode. That is when it has been quiet past its limit
// and the silence is not yet reported, or is reported but still owes a restart that may be made
// now. A silence is counted from the latest of its start, the last rearm and the moment the log
// became readable, so one the panel could not see is never held against the server, and a rearm
// delays a report without making a new episode of one already made.
func (h *hangWatch) silent(id string, p instance.Pulse, now time.Time) (anchor time.Time, ok bool) {
	now = now.Round(0)
	anchor, limit := p.Boot.Round(0), hangAfterStart
	if !p.Beat.IsZero() {
		anchor, limit = p.Beat.Round(0), hangAfterStatus
	}
	if now.Sub(latest(anchor, h.rearmed, p.Open.Round(0))) < limit {
		return anchor, false
	}
	if !h.wasReported(id, anchor) {
		return anchor, true
	}
	return anchor, h.owed[id].Equal(anchor) && h.mayRestart(id, now)
}

// latest is the last of the instants.
func latest(first time.Time, rest ...time.Time) time.Time {
	for _, t := range rest {
		if t.After(first) {
			first = t
		}
	}
	return first
}

// wasReported reports whether the silence that began at anchor has been announced.
func (h *hangWatch) wasReported(id string, anchor time.Time) bool {
	return h.reported[id].Equal(anchor)
}

// markReported records that the silence that began at anchor has been announced.
func (h *hangWatch) markReported(id string, anchor time.Time) {
	if h.reported == nil {
		h.reported = map[string]time.Time{}
	}
	h.reported[id] = anchor
}

// markOwed records that the restart the silence that began at anchor calls for is still to be made.
func (h *hangWatch) markOwed(id string, anchor time.Time) {
	if h.owed == nil {
		h.owed = map[string]time.Time{}
	}
	h.owed[id] = anchor
}

// mayRestart reports whether the watchdog has not restarted the server within restartCooldown.
func (h *hangWatch) mayRestart(id string, now time.Time) bool {
	last, ok := h.restarted[id]
	return !ok || now.Sub(last) >= restartCooldown
}

// markRestarted records a restart at now, which settles any restart the server was owed.
func (h *hangWatch) markRestarted(id string, now time.Time) {
	if h.restarted == nil {
		h.restarted = map[string]time.Time{}
	}
	h.restarted[id] = now
	delete(h.owed, id)
}

// firstNote reports whether this is the first time the silence that began at anchor is mentioned,
// so a condition that lasts many passes is logged once.
func (h *hangWatch) firstNote(id string, anchor time.Time) bool {
	if h.noted[id].Equal(anchor) {
		return false
	}
	if h.noted == nil {
		h.noted = map[string]time.Time{}
	}
	h.noted[id] = anchor
	return true
}

// watchdog reports every running server whose watchdog is on and whose log has stopped showing
// the game's status line, and restarts those whose watchdog says to. A restart waits for the
// next pass while another job is working on the server.
func (s *Supervisor) watchdog(ctx context.Context, now time.Time) {
	now = now.Round(0)
	if gap := s.hang.tick(now); gap > 0 {
		slog.WarnContext(ctx, "the watchdog's passes were far apart; silence is counted from now",
			slog.Duration("gap", gap))
	}
	instances, err := s.DB.ListInstances(ctx, nil)
	if err != nil {
		slog.WarnContext(ctx, "watchdog could not list instances", slog.Any("error", err))
		return
	}
	for i := range instances {
		inst := &instances[i]
		mode, pulse, ok := s.watched(inst)
		if !ok {
			continue
		}
		if anchor, hung := s.hang.silent(inst.ID, pulse, now); hung {
			s.handleHang(ctx, inst, hungServer{mode: mode, seen: !pulse.Beat.IsZero(), anchor: anchor}, now)
		}
	}
}

// watched is the watchdog's mode for inst and what its reader knows about it, or false when there
// is nothing to judge: the watchdog is off, the server is not running, or the panel is not reading
// its log, so that silence would say nothing about the server.
func (s *Supervisor) watched(inst *store.Instance) (instance.Watchdog, instance.Pulse, bool) {
	mode, known := instance.ParseWatchdog(inst.Watchdog)
	if !known || mode == instance.WatchdogOff || inst.State != string(instance.StateRunning) ||
		inst.ContainerID == nil {
		return mode, instance.Pulse{}, false
	}
	r := s.Streams.Reader(inst.ID)
	if r == nil {
		return mode, instance.Pulse{}, false
	}
	// One reading, so the boot, its heartbeat and its stream come from one boot.
	pulse := r.Pulse()
	return mode, pulse, !pulse.Boot.IsZero() && !pulse.Open.IsZero()
}

// hungServer is what the watchdog found out about one quiet server: its mode, whether it has
// logged a status line since its boot began, and when the silence began.
type hungServer struct {
	mode   instance.Watchdog
	seen   bool
	anchor time.Time
}

// handleHang reports a server that has gone quiet, and restarts it when its watchdog says to and
// it has not been restarted within the last hour. A restart that has to wait, for the hour or for
// another job, stays owed: the silence is reported once now and the restart is made, with a second
// report, as soon as it can be. A server that has logged no status line this boot is only reported:
// it may be loading a large world, how long that takes is not measured, and a restart would kill
// it in the middle. Nothing is done when the log itself says the server is alive.
func (s *Supervisor) handleHang(ctx context.Context, inst *store.Instance, hung hungServer, now time.Time) {
	first := !s.hang.wasReported(inst.ID, hung.anchor)
	if s.logShowsLife(ctx, inst, hung.anchor) {
		return
	}
	wantRestart := hung.mode == instance.WatchdogRestart && hung.seen
	if wantRestart && s.hang.mayRestart(inst.ID, now) && s.restartHung(ctx, inst) {
		s.hang.markRestarted(inst.ID, now)
		s.reportHang(ctx, inst, hung, instance.ReasonUnresponsiveRestarting)
		return
	}
	if !first {
		return
	}
	if wantRestart {
		s.hang.markOwed(inst.ID, hung.anchor)
	}
	s.reportHang(ctx, inst, hung, instance.ReasonUnresponsive)
}

// reportHang announces a hung server with reason, once for its silence.
func (s *Supervisor) reportHang(ctx context.Context, inst *store.Instance, hung hungServer, reason string) {
	s.hang.markReported(inst.ID, hung.anchor)
	slog.WarnContext(ctx, "server stopped logging its status line",
		slog.String("instance_id", inst.ID), slog.String("watchdog", string(hung.mode)),
		slog.Bool("seen_this_boot", hung.seen), slog.String("reason", reason))
	s.reportDown(ctx, inst, string(instance.StateRunning), reason)
}

// logShowsLife reports whether the pass should leave the server alone because the container's log
// carries a status line newer than the silence the log reader reported, which means the reader
// missed it, or because the log cannot be read and nothing is known. A reader whose stream stalled
// without closing would otherwise make a healthy server look hung.
func (s *Supervisor) logShowsLife(ctx context.Context, inst *store.Instance, anchor time.Time) bool {
	saw, err := s.Logs.SawStatusLineAfter(ctx, *inst.ContainerID, anchor)
	switch {
	case err != nil:
		if s.hang.firstNote(inst.ID, anchor) {
			slog.WarnContext(ctx, "the watchdog could not read the log to confirm a silence, so it waits",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
		}
		return true
	case saw:
		if s.hang.firstNote(inst.ID, anchor) {
			slog.WarnContext(ctx, "the log has a status line its reader never recorded, so the server is not hung",
				slog.String("instance_id", inst.ID))
		}
		return true
	}
	return false
}

// restartHung queues the restart of a hung server, forced so that a stop that ends in a kill
// still starts it, and reports whether it was queued.
func (s *Supervisor) restartHung(ctx context.Context, inst *store.Instance) bool {
	_, err := s.Restarter.Submit(ctx, &RestartSubmission{
		Instance: inst, ContainerID: *inst.ContainerID, ForceStart: true,
		Audit: &store.AuditEntry{
			InstanceID: inst.ID, Action: "instances.restart", ActorName: "Watchdog",
			Detail: `{"reason":"unresponsive"}`,
		},
	})
	if busy(err) {
		return false
	}
	if err != nil {
		slog.WarnContext(ctx, "watchdog restart not submitted",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return false
	}
	return true
}
