package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

func (s *Schedules) announce(sc *store.Schedule) {
	if s.Hub != nil && sc.InstanceID != nil {
		s.Hub.PublishMaintenance(*sc.InstanceID)
	}
}

func (s *Schedules) Occupied(ctx context.Context, sc *store.Schedule) bool {
	spec, ok := scheduleKinds[sc.Kind]
	if !ok || sc.InstanceID == nil {
		return false
	}
	if spec.chain {
		steps, err := control.ParseChainPayload(sc.Payload)
		if err != nil {
			return false
		}
		spec = spec.forSteps(steps)
	}
	if !spec.playerAware {
		return false
	}
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		slog.WarnContext(ctx, "scheduled run held: instance could not be read",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return true
	}
	if inst == nil {
		return false
	}
	var players *int
	if reader := s.Instances.Streams.Reader(inst.ID); reader != nil {
		players = reader.Players()
	}
	return occupied(instance.State(inst.State), players, sc.UnknownPlayers)
}

func (s *Schedules) Warn(ctx context.Context, sc *store.Schedule, left time.Duration) bool {
	if sc.InstanceID == nil {
		return true
	}
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		slog.WarnContext(ctx, "players not warned: instance could not be read",
			slog.String("schedule_id", sc.ID), slog.String("instance_id", *sc.InstanceID), slog.Any("error", err))
		return false
	}
	if inst == nil {
		return true
	}
	steps, err := scheduledSteps(sc)
	if err != nil {
		slog.WarnContext(ctx, "players not warned: the schedule's steps could not be read",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return true
	}
	return s.Instances.say(ctx, inst, playerWarning(steps, left))
}

func (h *Instances) say(ctx context.Context, inst *store.Instance, line string) bool {
	if h.Commands == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, warnTimeout)
	defer cancel()
	_, err := h.Commands.Send(ctx, inst, "say "+line)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "players warned", slog.String("instance_id", inst.ID), slog.String("line", line))
	case errors.Is(err, command.ErrUnsupported), errors.Is(err, command.ErrInvalidState):
	default:
		slog.WarnContext(ctx, "players could not be warned",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return false
	}
	return true
}

const warnTimeout = 15 * time.Second

// playerWarning is the line said to players when a run of steps is held for them to leave.
func playerWarning(steps []control.ChainStep, left time.Duration) string {
	action := "goes down for maintenance"
	if len(steps) == 1 {
		switch steps[0].Type {
		case control.StepRestart:
			action = "restarts"
		case control.StepBackup:
			action = "restarts for a backup"
		}
	}
	return fmt.Sprintf("The server %s when all players have left, or in %s at the latest.",
		action, spokenDuration(left))
}

func spokenDuration(d time.Duration) string {
	n, unit := int((d+time.Minute-1)/time.Minute), "minute"
	if n%60 == 0 {
		n, unit = n/60, "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

func occupied(state instance.State, players *int, unknown string) bool {
	if state != instance.StateRunning {
		return false
	}
	if players == nil {
		return unknown != store.UnknownPlayersRun
	}
	return *players > 0
}

func (s *Schedules) Enqueue(ctx context.Context, sc *store.Schedule) error {
	spec, ok := scheduleKinds[sc.Kind]
	if !ok {
		return fmt.Errorf("schedule %s names kind %q, which this build cannot run", sc.ID, sc.Kind)
	}
	if err := s.submitter().Enqueue(ctx, sc, spec.kind, spec.global); err != nil {
		return fmt.Errorf("enqueue schedule %s: %w", sc.ID, err)
	}
	return nil
}

func (s *Schedules) submitter() *scheduler.Submitter {
	return &scheduler.Submitter{
		DB:             s.DB,
		SubmitGlobal:   s.submitScheduledGlobal,
		SubmitInstance: s.submitScheduledInstance,
	}
}

func (s *Schedules) submitScheduledGlobal(ctx context.Context, sc *store.Schedule, kind jobs.Kind) error {
	switch kind {
	case jobs.KindPrune:
		_, err := s.Instances.ctl.Pruner.Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled prune: %w", err)
		}
		return nil
	case jobs.KindUpdateCheck:
		_, err := s.Instances.ctl.UpdateChecker.Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled update check: %w", err)
		}
		return nil
	case jobs.KindAlertScan:
		_, err := s.Instances.alerts.Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled alert scan: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("no runner for global kind %s", kind)
	}
}

// submitScheduledInstance submits what a due instance schedule runs. A chain runs as a chain
// job; a backup, restart or game update is its one step, submitted as that step's own job.
func (s *Schedules) submitScheduledInstance(ctx context.Context, sc *store.Schedule, inst *store.Instance) error {
	steps, err := scheduledSteps(sc)
	if err != nil {
		return err
	}
	chainer := s.Instances.ctl.Chainer
	if sc.Kind == jobs.KindChain.String() {
		_, err = chainer.Submit(ctx, &control.ChainSubmission{
			Instance: inst, Steps: steps, ScheduleID: sc.ID,
			Kind: jobs.KindChain, Payload: control.ChainPayload{Steps: steps},
		})
		return err //nolint:wrapcheck // keep the original skip reason
	}
	_, err = chainer.SubmitStep(ctx, inst, &steps[0], control.StepOrigin{ScheduleID: sc.ID})
	return err //nolint:wrapcheck // keep the original skip reason
}
