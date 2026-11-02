package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// wake signals whoever reads c without blocking the sender.
func wake(c chan<- struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// waitSignals is what the broker's publish tells a waiting goroutine: that the awaited job
// finished, and its latest progress message, which only the waiting goroutine passes to relay.
type waitSignals struct {
	relay  func(string)
	woke   chan struct{}
	moved  chan struct{}
	mu     sync.Mutex
	latest string
}

// observe takes one event of the awaited job without blocking the publisher.
func (s *waitSignals) observe(ev Event) {
	switch {
	case terminal(ev.Status):
		wake(s.woke)
	case s.relay != nil && ev.Message != "":
		s.mu.Lock()
		s.latest = ev.Message
		s.mu.Unlock()
		wake(s.moved)
	}
}

// idle relays progress until the job finishes or recheck fires. It returns ErrCancelRequested
// when cancel closes first, and ctx's error, naming job id, when ctx ends first.
func (s *waitSignals) idle(
	ctx context.Context, id string, cancel <-chan struct{}, recheck <-chan time.Time,
) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for job %s: %w", id, ctx.Err())
		case <-cancel:
			return ErrCancelRequested
		case <-s.moved:
			s.mu.Lock()
			message := s.latest
			s.mu.Unlock()
			s.relay(message)
		case <-s.woke:
			return nil
		case <-recheck:
			return nil
		}
	}
}

// wait blocks until job id reaches a terminal status and returns its row. It wakes on the job's
// finish event and re-reads the row periodically, so a job that recovery or a sweep ends without
// an event is still noticed. It returns ErrCancelRequested when cancel is closed; a nil cancel
// never fires. relay, when set, receives the job's progress messages on the waiting goroutine,
// never inside the broker's publish, starting with the one its row holds.
func (e *Engine) wait(
	ctx context.Context, id string, cancel <-chan struct{}, relay func(string),
) (*store.Job, error) {
	signals := &waitSignals{relay: relay, woke: make(chan struct{}, 1), moved: make(chan struct{}, 1)}
	stop := e.broker.Subscribe(id, signals.observe)
	defer stop()

	recheck := e.waitRecheck
	if recheck <= 0 {
		recheck = defaultWaitRecheck
	}
	timer := time.NewTimer(recheck)
	defer timer.Stop()
	for first := true; ; first = false {
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
		// A message the job wrote before the subscription is relayed from its row.
		if first && relay != nil && row.Message != nil && *row.Message != "" {
			relay(*row.Message)
		}
		timer.Reset(recheck)
		if err := signals.idle(ctx, id, cancel, timer.C); err != nil {
			return nil, err
		}
	}
}

// WaitJob waits for a job this one started to reach a terminal status and returns its row. It returns ErrCancelRequested when this
// job is asked to cancel; the awaited job is left running. While it waits, this job's progress
// message carries the awaited job's after its own, so a long child shows what it is doing.
func (h *Handle) WaitJob(ctx context.Context, id string) (*store.Job, error) {
	if h.CancelRequested(ctx) {
		return nil, ErrCancelRequested
	}
	h.mu.Lock()
	pct, own := h.progress, h.message
	h.mu.Unlock()
	relayed := false
	relay := func(child string) {
		relayed = true
		if own != "" {
			child = own + ": " + child
		}
		h.Progress(ctx, pct, child)
	}
	row, err := h.engine.wait(ctx, id, h.cancelWake, relay)
	if relayed {
		// Not cancelled with ctx, so a job interrupted mid-wait does not keep the child's text.
		h.Progress(context.WithoutCancel(ctx), pct, own)
	}
	return row, err
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
