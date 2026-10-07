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
	// ModQueue may be nil, in which case a restart ignores queued mod installs.
	ModQueue *ModQueue
}

// Run stops the server and starts it again. With mod installs queued it stops there instead,
// and the mod queue installs them and gives the server the start it is owed.
func (r *Restarter) Run(inst *store.Instance, containerID, requestedBy string) jobs.Runner {
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
		var queued []store.QueuedModInstall
		if r.ModQueue != nil {
			if queued, err = r.Starter.DB.QueuedModInstalls(ctx, instanceID); err != nil {
				jh.Log(fmt.Sprintf("warning: could not read the mod install queue, restarting without it: %v", err))
			}
		}
		if len(queued) > 0 {
			return r.stopForQueue(ctx, jh, instanceID, requestedBy, len(queued), clean, archived, pruneCleanup)
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

// stopForQueue finishes a restart as a stop that owes the server a start, so the mod queue
// can install between the two.
func (r *Restarter) stopForQueue(
	ctx context.Context, jh *jobs.Handle, instanceID, requestedBy string, queued int, clean bool,
	archived func(context.Context, *sql.Tx) error, pruneCleanup func(context.Context),
) jobs.Outcome {
	settleConfigs(ctx, r.Starter.DB, jh, instanceID)
	jh.Progress(ctx, 100, fmt.Sprintf("stopped; installing %d queued mod(s), then starting", queued))
	finish := func(ctx context.Context, tx *sql.Tx) error {
		ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateStopping, instance.StateStopped)
		if err != nil {
			return fmt.Errorf("finish restart of instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish restart of instance %s: not in stopping state", instanceID)
		}
		return store.TxQueueModStart(ctx, tx, instanceID, requestedBy)
	}
	if archived != nil {
		finish = chainFinish(finish, archived)
	}
	return jobs.Outcome{
		Status: jobs.StatusSucceeded, Clean: &clean, OnFinish: finish, AfterFinish: pruneCleanup,
	}
}
