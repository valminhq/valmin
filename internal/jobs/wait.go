package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// ErrCancelRequested is what a Handle's waits return once the job has been asked to cancel.
var ErrCancelRequested = errors.New("job cancel requested")

// defaultWaitRecheck is how often a wait re-reads a row it expects an event for.
const defaultWaitRecheck = 30 * time.Second

// terminal reports whether status is one a job never leaves.
func terminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled
}

// wait blocks until job id reaches a terminal status and returns its row. It wakes on the job's
// finish event and re-reads the row periodically, so a job that recovery or a sweep ends without
// an event is still noticed. It returns ErrCancelRequested when cancel is closed; a nil cancel
// never fires.
func (e *Engine) wait(ctx context.Context, id string, cancel <-chan struct{}) (*store.Job, error) {
	woke := make(chan struct{}, 1)
	stop := e.broker.Subscribe(id, func(ev Event) {
		if !terminal(ev.Status) {
			return
		}
		select {
		case woke <- struct{}{}:
		default:
		}
	})
	defer stop()

	recheck := e.waitRecheck
	if recheck <= 0 {
		recheck = defaultWaitRecheck
	}
	timer := time.NewTimer(recheck)
	defer timer.Stop()
	for {
		row, err := e.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, ErrJobNotFound
		}
		if terminal(row.Status) {
			return row, nil
		}
		timer.Reset(recheck)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for job %s: %w", id, ctx.Err())
		case <-cancel:
			return nil, ErrCancelRequested
		case <-woke:
		case <-timer.C:
		}
	}
}

// WaitJob waits for a job this one started to reach a terminal status and returns its row. It returns ErrCancelRequested when this
// job is asked to cancel; the awaited job is left running.
func (h *Handle) WaitJob(ctx context.Context, id string) (*store.Job, error) {
	if h.CancelRequested(ctx) {
		return nil, ErrCancelRequested
	}
	return h.engine.wait(ctx, id, h.cancelWake)
}

// Sleep waits d, returning ErrCancelRequested as soon as this job is asked to cancel.
func (h *Handle) Sleep(ctx context.Context, d time.Duration) error {
	if h.CancelRequested(ctx) {
		return ErrCancelRequested
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait: %w", ctx.Err())
	case <-h.cancelWake:
		return ErrCancelRequested
	case <-t.C:
		return nil
	}
}

// wakeCancel tells a running job's waits that it was asked to cancel.
func (e *Engine) wakeCancel(jobID string) {
	e.mu.Lock()
	h := e.handles[jobID]
	e.mu.Unlock()
	if h != nil {
		h.cancelOnce.Do(func() { close(h.cancelWake) })
	}
}
