package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// bootLatch records which events one boot of a container has logged, and whether its log
// stream has ended. A waiter takes a channel per event kind and is woken the moment the reader
// matches the line, so nothing polls the log.
type bootLatch struct {
	mu     sync.Mutex
	boot   time.Time
	seen   map[EventKind]chan struct{}
	fired  map[EventKind]bool
	exited chan struct{}
	ended  bool
}

// newBootLatch returns the record of a boot that has logged nothing yet.
func newBootLatch() *bootLatch {
	return &bootLatch{
		seen:   make(map[EventKind]chan struct{}),
		fired:  make(map[EventKind]bool),
		exited: make(chan struct{}),
	}
}

// channel returns the channel closed when kind has been logged this boot.
func (b *bootLatch) channel(kind EventKind) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.channelLocked(kind)
}

// channelLocked is channel for a caller that holds b.mu.
func (b *bootLatch) channelLocked(kind EventKind) chan struct{} {
	ch, ok := b.seen[kind]
	if !ok {
		ch = make(chan struct{})
		b.seen[kind] = ch
	}
	return ch
}

// note records that kind was logged.
func (b *bootLatch) note(kind EventKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fired[kind] {
		return
	}
	b.fired[kind] = true
	close(b.channelLocked(kind))
}

// setBoot records when the boot this latch describes began.
func (b *bootLatch) setBoot(at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.boot = at
}

// bootStart is when the boot this latch describes began, or zero before the reader knows.
func (b *bootLatch) bootStart() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.boot
}

// end records that the log stream of this boot has ended.
func (b *bootLatch) end() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ended {
		b.ended = true
		close(b.exited)
	}
}

// AwaitReady waits for the container's current boot to log its ready line and reports whether
// it did. A container still running when settle ends is ready but unconfirmed: false with no
// error. Only a container that exits first, or ctx, is an error. The container must carry the
// instance label.
func (l *Streams) AwaitReady(ctx context.Context, containerID string, settle time.Duration) (bool, error) {
	c, err := l.rt.Inspect(ctx, containerID)
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	return l.await(ctx, &c, readyEvent(&c), settle)
}

// AwaitPluginLoad reports whether a modded server's chainloader logged its plugin count within
// window. A misconfigured chainloader boots cleanly and loads nothing, so an absent line is the
// only evidence there is; false is a warning, never a failure.
func (l *Streams) AwaitPluginLoad(ctx context.Context, containerID string, window time.Duration) bool {
	c, err := l.rt.Inspect(ctx, containerID)
	if err != nil {
		slog.WarnContext(ctx, "could not inspect the container while checking for a plugin count",
			slog.String("container_id", containerID), slog.Any("error", err))
		return false
	}
	seen, err := l.await(ctx, &c, EventPluginCount, window)
	if err != nil {
		slog.WarnContext(ctx, "could not read the log while checking for a plugin count",
			slog.String("container_id", containerID), slog.Any("error", err))
		return false
	}
	return seen
}

// readinessPollInterval paces the log reads of a wait that cannot use the reader, and the
// look-again after a reader stopped under a wait.
const readinessPollInterval = 500 * time.Millisecond

// await waits for kind in the container's current boot, reading the log through the instance's
// reader. If the reader is stopped or is still on an earlier boot it is started again, and the
// wait goes on for the rest of settle. A line is accepted only from a container that is still
// running.
func (l *Streams) await(ctx context.Context, c *runtime.Container, kind EventKind, settle time.Duration) (bool, error) {
	instanceID := c.Labels[LabelInstanceID]
	if instanceID == "" {
		return false, fmt.Errorf("container %s carries no instance label", c.ID)
	}
	timer := time.NewTimer(settle)
	defer timer.Stop()
	for retry := false; ; retry = true {
		if retry {
			fresh, err := l.rt.Inspect(ctx, c.ID)
			if err != nil {
				return false, fmt.Errorf("inspect container %s: %w", c.ID, err)
			}
			c = &fresh
		}
		seen, done, err := l.awaitBoot(ctx, instanceID, c, kind, timer.C)
		if done || err != nil {
			return seen, err
		}
	}
}

// awaitBoot is one pass of await over the reader's record of c's current boot. done is false when
// the reader stopped under the wait while the container still runs, so the caller looks again.
func (l *Streams) awaitBoot(
	ctx context.Context, instanceID string, c *runtime.Container, kind EventKind, limit <-chan time.Time,
) (seen, done bool, err error) {
	if l.isShutdown() {
		seen, err = l.awaitPolled(ctx, c, kind, limit)
		return seen, true, err
	}
	latch := l.Open(instanceID, c.ID).latch() //nolint:contextcheck // the reader outlives this wait
	if at := latch.bootStart(); !at.IsZero() && !at.Equal(bootOf(c)) {
		latch = l.reopen(instanceID, c.ID).latch() //nolint:contextcheck // as above
	}
	select {
	case <-latch.channel(kind):
		err = l.running(ctx, c.ID)
		return err == nil, true, err
	case <-latch.exited:
		select {
		case <-latch.channel(kind):
			err = l.running(ctx, c.ID)
			return err == nil, true, err
		default:
		}
		if err := l.running(ctx, c.ID); err != nil {
			return false, true, err
		}
		select {
		case <-ctx.Done():
			return false, true, fmt.Errorf("await %s of container %s: %w", kind, c.ID, ctx.Err())
		case <-limit:
			return false, true, l.running(ctx, c.ID)
		case <-time.After(readinessPollInterval):
			return false, false, nil
		}
	case <-limit:
		return false, true, l.running(ctx, c.ID)
	case <-ctx.Done():
		return false, true, fmt.Errorf("await %s of container %s: %w", kind, c.ID, ctx.Err())
	}
}

// awaitPolled is await for streams that have been shut down: it reads the container's log
// directly until the line is seen or limit fires.
func (l *Streams) awaitPolled(
	ctx context.Context, c *runtime.Container, kind EventKind, limit <-chan time.Time,
) (bool, error) {
	logs := NewContainerLogs(l.rt, l.patterns)
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()
	for {
		now, err := l.rt.Inspect(ctx, c.ID)
		if err != nil {
			return false, fmt.Errorf("inspect container %s: %w", c.ID, err)
		}
		seen, err := logs.matches(ctx, c.ID, kind, bootOf(&now))
		if err != nil {
			return false, err
		}
		if seen {
			err = l.running(ctx, c.ID)
			return err == nil, err
		}
		if !now.Running {
			return false, fmt.Errorf("container exited with code %d before becoming ready", now.ExitCode)
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("await %s of container %s: %w", kind, c.ID, ctx.Err())
		case <-limit:
			return false, nil
		case <-ticker.C:
		}
	}
}

// running returns an error when the container is not running: a line says the server got that
// far, never that it is still there.
func (l *Streams) running(ctx context.Context, containerID string) error {
	c, err := l.rt.Inspect(ctx, containerID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return fmt.Errorf("container %s is gone before becoming ready", containerID)
	case err != nil:
		return fmt.Errorf("inspect container %s: %w", containerID, err)
	case !c.Running:
		return fmt.Errorf("container exited with code %d before becoming ready", c.ExitCode)
	}
	return nil
}
