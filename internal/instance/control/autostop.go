package control

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// idleClock tracks, per instance, since when a running server has had a known count of zero
// players. It is held in memory only: a daemon restart starts every clock again.
type idleClock map[string]time.Time

// observe folds one observation in and reports whether the server has now been empty for limit.
// An unknown count counts as occupied, so a server whose log the panel cannot read is never
// stopped on a guess.
func (c idleClock) observe(id string, running bool, players *int, limit time.Duration, now time.Time) bool {
	if !running || limit <= 0 || players == nil || *players > 0 {
		delete(c, id)
		return false
	}
	since, ok := c[id]
	if !ok {
		c[id] = now
		return false
	}
	return now.Sub(since) >= limit
}

// autoStop submits a stop for every running instance whose auto-stop delay has passed with no
// players connected. A job already holding the instance defers the stop to a later tick.
func (s *Supervisor) autoStop(ctx context.Context, now time.Time) {
	instances, err := s.DB.ListInstances(ctx, nil)
	if err != nil {
		slog.WarnContext(ctx, "auto-stop could not list instances", slog.Any("error", err))
		return
	}
	for i := range instances {
		inst := &instances[i]
		limit := time.Duration(inst.AutoStopMinutes) * time.Minute
		running := inst.State == string(instance.StateRunning) && inst.ContainerID != nil
		if !s.idle.observe(inst.ID, running, s.players(inst.ID), limit, now) {
			continue
		}
		s.submitAutoStop(ctx, inst)
	}
}

// players is the instance's live player count, nil when the panel cannot say.
func (s *Supervisor) players(instanceID string) *int {
	if r := s.Streams.Reader(instanceID); r != nil {
		return r.Players()
	}
	return nil
}

// submitAutoStop queues the stop and records who asked for it.
func (s *Supervisor) submitAutoStop(ctx context.Context, inst *store.Instance) {
	_, err := s.Stopper.Submit(ctx, &StopSubmission{
		Instance: inst, ContainerID: *inst.ContainerID, Reason: instance.StopNoPlayers,
		Audit: &store.AuditEntry{
			InstanceID: inst.ID, Action: "instances.stop", ActorName: "Auto-stop",
			Detail: fmt.Sprintf(`{"reason":"no_players","minutes":%d}`, inst.AutoStopMinutes),
		},
	})
	if busy(err) {
		return
	}
	delete(s.idle, inst.ID)
	if err != nil {
		slog.WarnContext(ctx, "auto-stop not submitted",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "auto-stop submitted",
		slog.String("instance_id", inst.ID), slog.Int("idle_minutes", inst.AutoStopMinutes))
}
