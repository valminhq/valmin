package control

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
)

// Stopper executes the stop phase shared by stop, restart, and quiesced backup jobs.
type Stopper struct {
	Runtime      runtime.Runtime
	StopTimeout  time.Duration
	ReadyTimeout time.Duration
}

// Run stops a container and records the resulting instance state in the job transaction.
func (s Stopper) Run(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 30, "stopping container")
		clean, timedOut, err := s.StopContainer(ctx, containerID)
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error(),
				OnFinish: finishToError(instanceID, instance.StateStopping),
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
		msg := "stopped"
		if !clean {
			msg = "stopped (world save not confirmed)"
		}
		jh.Progress(ctx, 100, msg)
		cleanCopy := clean
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			Clean:  &cleanCopy,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateStopping, instance.StateStopped)
				if err != nil {
					return fmt.Errorf("finish stop for instance %s: %w", instanceID, err)
				}
				if !ok {
					return fmt.Errorf("finish stop for instance %s: not in stopping state", instanceID)
				}
				return nil
			},
		}
	}
}

// StopContainer sends SIGINT and reports whether a save-complete line was seen and whether
// Docker had to escalate. The save evidence starts at the signal, excluding earlier autosaves.
func (s Stopper) StopContainer(ctx context.Context, containerID string) (clean, timedOut bool, err error) {
	s.awaitSignalHonoured(ctx, containerID)
	start := time.Now()
	if err := s.Runtime.Stop(ctx, containerID, "SIGINT", s.StopTimeout); err != nil {
		return false, false, fmt.Errorf("stop container: %w", err)
	}
	if time.Since(start) >= s.StopTimeout {
		return false, true, nil
	}
	seenClean, saveErr := instance.SawSaveLine(ctx, s.Runtime, containerID, start)
	if saveErr != nil {
		//nolint:nilerr // The stop succeeded; unreadable logs only leave the save unconfirmed.
		return false, false, nil
	}
	return seenClean, false, nil
}

// awaitSignalHonoured waits through the early boot window where SIGINT may not reach the
// game's save path. Inspection or readiness failures still allow an operator's stop to proceed.
func (s Stopper) awaitSignalHonoured(ctx context.Context, containerID string) {
	c, err := s.Runtime.Inspect(ctx, containerID)
	if err != nil || !c.Running || c.StartedAt.IsZero() {
		return
	}
	left := s.ReadyTimeout - time.Since(c.StartedAt)
	if left <= 0 {
		return
	}
	if _, err := instance.AwaitReady(ctx, s.Runtime, containerID, left, left); err != nil {
		slog.WarnContext(ctx, "stopping without confirming the server finished starting",
			slog.String("container_id", containerID), slog.Any("error", err))
	}
}
