package jobs

import (
	"context"
	"fmt"
	"time"
)

// CancelPolicy reports whether a job at checkpoint may still be cancelled, and names the
// phase for the 409 when it may not. 12 §8: "points of no return are declared per kind,
// not discovered" — the engine has no opinion of its own, it only enforces whatever the
// kind declares.
type CancelPolicy func(checkpoint string) (cancellable bool, phase string)

// RegisterCancelPolicy attaches k's policy. A kind with none registered is never
// cancellable once running: 12 §8's table gives start/stop/restart/delete no interruptible
// phase at all, being seconds long.
func (e *Engine) RegisterCancelPolicy(k Kind, p CancelPolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policies[k] = p
}

// Cancel is POST /jobs/{id}/cancel (12 §8). It only ever sets cancel_requested_at or, for
// a still-queued row, cancels it outright — stopping the actual work is the Runner's
// cooperative job, checked via Handle.CancelRequested at its kind's declared boundaries.
func (e *Engine) Cancel(ctx context.Context, jobID string) error {
	j, err := e.db.JobByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("cancel job %s: %w", jobID, err)
	}
	if j == nil {
		return ErrJobNotFound
	}

	switch j.Status {
	case "queued":
		ok, err := e.db.CancelQueuedJob(ctx, jobID, time.Now())
		if err != nil {
			return fmt.Errorf("cancel job %s: %w", jobID, err)
		}
		if !ok {
			// Lost the race to a worker that claimed it between the read above and here.
			return ErrJobTerminal
		}
		return nil

	case "running":
		checkpoint := ""
		if j.Checkpoint != nil {
			checkpoint = *j.Checkpoint
		}
		e.mu.Lock()
		policy, ok := e.policies[Kind{j.Kind}]
		e.mu.Unlock()
		if !ok {
			return &ErrNotCancellable{Phase: j.Status}
		}
		cancellable, phase := policy(checkpoint)
		if !cancellable {
			return &ErrNotCancellable{Phase: phase}
		}
		if err := e.db.RequestJobCancel(ctx, jobID, time.Now()); err != nil {
			return fmt.Errorf("cancel job %s: %w", jobID, err)
		}
		return nil

	default:
		return ErrJobTerminal
	}
}
