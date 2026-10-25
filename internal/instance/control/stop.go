package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Stopper executes the stop phase shared by stop, restart, and quiesced backup jobs.
type Stopper struct {
	DB           *store.DB
	Engine       *jobs.Engine
	Runtime      runtime.Runtime
	Streams      *instance.Streams
	Logs         *instance.ContainerLogs
	StopTimeout  time.Duration
	ReadyTimeout time.Duration
}

// Run stops a container and records the resulting instance state in the job transaction.
func (s *Stopper) Run(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 30, "stopping container")
		clean, killed, err := s.stopContainer(ctx, containerID)
		if err == nil {
			settleConfigs(ctx, s.DB, jh, instanceID)
		}
		if out := stopFailure(jh, instanceID, killed, err); out != nil {
			return *out
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

// stopFailure is the outcome of a stop that failed or that Docker had to force-kill, which
// parks the instance in error; nil means the stop ended and the caller carries on.
func stopFailure(jh *jobs.Handle, instanceID string, killed bool, err error) *jobs.Outcome {
	switch {
	case err != nil:
		return &jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error(),
			OnFinish: finishToError(instanceID, instance.StateStopping),
		}
	case killed:
		jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
		return &jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error:    "the server did not stop within the timeout and was force-killed",
			OnFinish: finishToError(instanceID, instance.StateStopping),
		}
	}
	return nil
}

// stopContainer sends SIGINT and reports whether a save-complete line was seen and whether
// Docker had to force-kill the container. The save evidence starts at the signal, excluding
// earlier autosaves.
func (s *Stopper) stopContainer(ctx context.Context, containerID string) (clean, killed bool, err error) {
	if s.StopTimeout <= 0 {
		return false, false, errors.New("stop container: no stop timeout configured, refusing to kill without a save")
	}
	s.awaitSignalHonoured(ctx, containerID)
	start := time.Now()
	if err := s.Runtime.Stop(ctx, containerID, "SIGINT", s.StopTimeout); err != nil {
		return false, false, fmt.Errorf("stop container: %w", err)
	}
	if s.forceKilled(ctx, containerID, start) {
		return false, true, nil
	}
	seenClean, saveErr := s.Logs.SawSaveLine(ctx, containerID, start)
	if saveErr != nil {
		//nolint:nilerr // The stop succeeded; unreadable logs only leave the save unconfirmed.
		return false, false, nil
	}
	return seenClean, false, nil
}

// forceKilled reports whether the stop ended in Docker's SIGKILL: exit 137 without the kernel's
// OOM kill. Without an inspection it falls back to whether the stop took the whole timeout.
func (s *Stopper) forceKilled(ctx context.Context, containerID string, start time.Time) bool {
	c, err := s.Runtime.Inspect(ctx, containerID)
	if err != nil || c.Running {
		return time.Since(start) >= s.StopTimeout
	}
	return c.ExitCode == 137 && !c.OOMKilled
}

// awaitSignalHonoured waits through the early boot window where SIGINT may not reach the
// game's save path. Inspection or readiness failures still allow an operator's stop to proceed.
func (s *Stopper) awaitSignalHonoured(ctx context.Context, containerID string) {
	c, err := s.Runtime.Inspect(ctx, containerID)
	if err != nil || !c.Running || c.StartedAt.IsZero() {
		return
	}
	left := s.ReadyTimeout - time.Since(c.StartedAt)
	if left <= 0 {
		return
	}
	if _, err := s.Streams.AwaitReady(ctx, containerID, left); err != nil {
		slog.WarnContext(ctx, "stopping without confirming the server finished starting",
			slog.String("container_id", containerID), slog.Any("error", err))
	}
}
