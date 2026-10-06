package control

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Restarter owns the stop-to-start transition. Archive is an optional snapshot callback.
type Restarter struct {
	Engine    *jobs.Engine
	Starter   *Starter
	Stopper   *Stopper
	Backupper *Backupper
}

func (r *Restarter) Run(inst *store.Instance, containerID string) jobs.Runner {
	instanceID := inst.ID
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "stopping container")
		clean, timedOut, err := r.Stopper.stopContainer(ctx, containerID)
		cleanCopy := clean
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
				Error: err.Error(), OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		if timedOut {
			jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
				Error:    "the server did not stop within the timeout and was force-killed",
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		var archived func(context.Context, *sql.Tx) error
		var pruneCleanup func(context.Context)
		if r.Backupper != nil {
			archived, pruneCleanup = r.Backupper.archiveOnRestart(ctx, jh, inst, clean)
		}
		if _, err := instance.SetState(
			ctx,
			r.Starter.DB,
			instanceID,
			instance.StateStopping,
			instance.StateStarting,
		); err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
				Error: fmt.Sprintf("move instance %s to starting: %v", instanceID, err), Clean: &cleanCopy,
			}
		}
		jh.Progress(ctx, 50, "starting container")
		outcome := r.Starter.startAndAwaitReady(ctx, jh, instanceID, containerID)
		outcome.Clean = &cleanCopy
		if archived != nil && outcome.Status == jobs.StatusSucceeded {
			outcome.OnFinish = chainFinish(outcome.OnFinish, archived)
			outcome.AfterFinish = chainAfterFinish(pruneCleanup, outcome.AfterFinish)
		}
		return outcome
	}
}
