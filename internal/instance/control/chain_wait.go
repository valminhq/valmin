package control

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// stateRecheck is how often a state wait re-reads the instance when no change was announced.
const stateRecheck = 30 * time.Second

// stateWatch tells waiters that an instance's state may have changed. A nil watch announces
// nothing.
type stateWatch struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

// notify wakes every waiter on instanceID.
func (w *stateWatch) notify(instanceID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.waiters[instanceID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// subscribe returns a channel that receives after each change to instanceID, and the function
// that ends the subscription.
func (w *stateWatch) subscribe(instanceID string) (changed <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	if w == nil {
		return ch, func() {}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiters == nil {
		w.waiters = make(map[string]map[chan struct{}]struct{})
	}
	if w.waiters[instanceID] == nil {
		w.waiters[instanceID] = make(map[chan struct{}]struct{})
	}
	w.waiters[instanceID][ch] = struct{}{}
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.waiters[instanceID], ch)
		if len(w.waiters[instanceID]) == 0 {
			delete(w.waiters, instanceID)
		}
	}
}

// waitChild waits for a step's job to end. A cancel of the chain ends the wait and leaves the
// step's job running.
func (c *Chainer) waitChild(ctx context.Context, jh *jobs.Handle, job *store.Job) error {
	row, err := jh.WaitJob(ctx, job.ID)
	if err != nil {
		return err //nolint:wrapcheck // the wait names the job
	}
	if row.Status == jobs.StatusSucceeded {
		return nil
	}
	return &childJobError{
		kind: row.Kind, status: row.Status, code: deref(row.ErrorCode), message: deref(row.Error),
	}
}

// awaitState waits for the instance to reach want, and fails once it is in error or the settle
// time has passed.
func (c *Chainer) awaitState(ctx context.Context, jh *jobs.Handle, id string, want instance.State) error {
	settle := c.Settle
	if settle <= 0 {
		settle = defaultChainSettle
	}
	changed, stop := c.states.subscribe(id)
	defer stop()
	deadline := time.NewTimer(settle)
	defer deadline.Stop()
	recheck := time.NewTicker(stateRecheck)
	defer recheck.Stop()
	for {
		inst, err := c.fresh(ctx, id)
		if err != nil {
			return err
		}
		state := instance.State(inst.State)
		if state == want {
			return nil
		}
		if state == instance.StateError {
			return fmt.Errorf("%w: waiting for %s, it is %s", errWrongState, want, state)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", want, ctx.Err())
		case <-jh.Cancelled():
			return jobs.ErrCancelRequested
		case <-deadline.C:
			return fmt.Errorf("%w: waiting for %s, it is %s", errWrongState, want, inst.State)
		case <-changed:
		case <-recheck.C:
		}
	}
}

// cancelOnRequest returns a context that ends when ctx does or when jh is asked to cancel, for a
// wait that only watches its context. The returned function releases it.
func cancelOnRequest(parent context.Context, jh *jobs.Handle) (ctx context.Context, release func()) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-jh.Cancelled():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
