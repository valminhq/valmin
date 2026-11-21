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
	return s.Instances.say(ctx, inst, control.PlayerWarning(steps, left))
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

// submitScheduledInstance submits what a due instance schedule runs.
func (s *Schedules) submitScheduledInstance(ctx context.Context, sc *store.Schedule, inst *store.Instance) error {
	_, err := s.submitSchedule(ctx, sc, inst, control.StepOrigin{ScheduleID: sc.ID})
	return err
}

// submitSchedule submits what instance schedule sc runs on inst, attributed to origin. A chain
// runs as a chain job; a backup, restart or game update is its one step, submitted as that
// step's own job.
func (s *Schedules) submitSchedule(
	ctx context.Context, sc *store.Schedule, inst *store.Instance, origin control.StepOrigin,
) (*store.Job, error) {
	if sc.Kind == jobs.KindUpdateTest.String() {
		return s.submitUpdateTest(ctx, sc, inst, origin)
	}
	steps, err := scheduledSteps(sc)
	if err != nil {
		return nil, err
	}
	chainer := s.Instances.ctl.Chainer
	if sc.Kind == jobs.KindChain.String() {
		return chainer.Submit(ctx, &control.ChainSubmission{
			Instance: inst, Steps: steps, ScheduleID: origin.ScheduleID, RequestedBy: origin.RequestedBy,
			Audit: origin.Audit, Kind: jobs.KindChain, Payload: control.ChainPayload{Steps: steps},
		})
	}
	return chainer.SubmitStep(ctx, inst, &steps[0], origin)
}

// errNothingToTest is a scheduled update test with no game or mod update to try.
var errNothingToTest = errors.New("no game or mod updates are pending")

// submitUpdateTest queues the update test schedule sc runs on inst: the game unless the last
// check found it current, and the mods when any update is pending. With neither it refuses with
// errNothingToTest.
func (s *Schedules) submitUpdateTest(
	ctx context.Context, sc *store.Schedule, inst *store.Instance, origin control.StepOrigin,
) (*store.Job, error) {
	status, err := observeUpdateStatus(ctx, s.DB, inst)
	if err != nil {
		return nil, err
	}
	tester := s.Instances.ctl.UpdateTester
	mods, err := tester.PendingMods(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	game := status.UpdateAvailable == nil || *status.UpdateAvailable
	if !game && len(mods) == 0 {
		return nil, errNothingToTest
	}
	job, err := tester.Submit(ctx, &control.UpdateTestSubmission{
		Instance: inst, Game: game, Mods: len(mods) > 0, Apply: scheduledApply(sc),
		RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID, Audit: origin.Audit,
	})
	if err != nil {
		return nil, fmt.Errorf("submit scheduled update test: %w", err)
	}
	return job, nil
}
