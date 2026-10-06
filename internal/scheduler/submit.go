package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Submitter turns due schedules into jobs or recorded skips.
type Submitter struct {
	DB             *store.DB
	SubmitGlobal   func(context.Context, *store.Schedule, jobs.Kind) error
	SubmitInstance func(context.Context, *store.Schedule, jobs.Kind, *store.Instance, string) error
}

// Enqueue submits the job sc asks for, or records why the run was skipped.
func (s *Submitter) Enqueue(ctx context.Context, sc *store.Schedule, kind jobs.Kind, global bool) error {
	if global {
		err := s.SubmitGlobal(ctx, sc, kind)
		if err == nil {
			return nil
		}
		return s.recordSkip(ctx, sc, kind, nil, err)
	}
	if sc.InstanceID == nil {
		return fmt.Errorf("schedule %s of kind %s names no instance", sc.ID, sc.Kind)
	}
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		return fmt.Errorf("read instance %s: %w", *sc.InstanceID, err)
	}
	if inst == nil {
		return nil
	}
	if !claimableFrom(kind, instance.State(inst.State)) {
		return s.recordSkip(
			ctx,
			sc,
			kind,
			inst,
			fmt.Errorf("the instance was %s, which %s cannot run from", inst.State, kind),
		)
	}
	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}
	if err := s.SubmitInstance(ctx, sc, kind, inst, containerID); err != nil {
		return s.recordSkip(ctx, sc, kind, inst, err)
	}
	return nil
}

func claimableFrom(kind jobs.Kind, state instance.State) bool {
	for _, allowed := range instance.AllowedFrom(kind) {
		if state == allowed {
			return true
		}
	}
	return false
}

func (s *Submitter) recordSkip(
	ctx context.Context,
	sc *store.Schedule,
	kind jobs.Kind,
	inst *store.Instance,
	cause error,
) error {
	code := errcode.Internal.String()
	var conflict *store.JobConflict
	if errors.As(cause, &conflict) {
		code = errcode.JobInProgress.String()
	}
	row := &store.Job{ID: store.NewID(), Kind: kind.String(), ScheduleID: &sc.ID, LockKey: jobs.GlobalLockKey(kind)}
	if inst != nil {
		row.InstanceID = &inst.ID
		row.InstanceName = inst.Name
		row.LockKey = jobs.InstanceLockKey(inst.ID)
	}
	if err := s.DB.RecordSkippedRun(ctx, row, code, "This scheduled run was skipped: "+cause.Error()); err != nil {
		return fmt.Errorf("record skipped run of schedule %s: %w", sc.ID, err)
	}
	slog.InfoContext(
		ctx,
		"scheduled run skipped",
		slog.String("schedule_id", sc.ID),
		slog.String("kind", kind.String()),
		slog.String("reason", cause.Error()),
	)
	return nil
}
