package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/valminhq/valmin/internal/runtime"
)

// readinessPollInterval paces AwaitReady's polling of the log and the container's running
// state. A plain poll, not a live follow: the low-latency, ring-buffered reader every other
// consumer shares is WP-19's, and a job only ever needs a yes/no answer a handful of times
// over jobs.ready_settle/ready_timeout.
const readinessPollInterval = 500 * time.Millisecond

// AwaitReady is 12 §3.3's readiness wait. confirmed reports which path was taken: true for
// the primary path (the anchored line seen), false for the fallback (still running, no
// exit, ready_settle elapsed with no line) — ADR-043 makes the fallback a warning, not a
// failure, so callers must not treat confirmed == false as an error. err is set only when
// the container exits before either path resolves, the readiness deadline is exceeded, or
// ctx is done.
func AwaitReady(
	ctx context.Context,
	rt runtime.Runtime,
	containerID string,
	settle, timeout time.Duration,
) (bool, error) {
	settleDeadline := time.Now().Add(settle)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()

	for {
		seen, err := containerLogMatches(ctx, rt, containerID, EventReady)
		if err != nil {
			return false, err
		}
		if seen {
			return true, nil
		}

		c, err := rt.Inspect(ctx, containerID)
		if err != nil {
			return false, fmt.Errorf("inspect container %s: %w", containerID, err)
		}
		if !c.Running {
			return false, fmt.Errorf("container exited with code %d before becoming ready", c.ExitCode)
		}

		now := time.Now()
		if now.After(settleDeadline) {
			return false, nil
		}
		if now.After(deadline) {
			return false, errors.New("readiness timeout exceeded")
		}

		select {
		case <-ctx.Done():
			return false, fmt.Errorf("await readiness of container %s: %w", containerID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// SawSaveLine reports whether containerID's full log contains the save-complete literal —
// checked once, after the container has exited (12 §3.4), never while it might still be
// writing the line.
func SawSaveLine(ctx context.Context, rt runtime.Runtime, containerID string) (bool, error) {
	return containerLogMatches(ctx, rt, containerID, EventSaveComplete)
}

// LogTail returns containerID's last n demuxed log lines, for attaching to a failed job's
// log (12 §7's "last N log lines attached").
func LogTail(ctx context.Context, rt runtime.Runtime, containerID string, n int) (string, error) {
	return readLog(ctx, rt, containerID, n)
}

// containerLogMatches reports whether containerID's log carries a line of the given kind,
// matched through 14 §4.5's one pattern set rather than a literal of this file's own.
func containerLogMatches(ctx context.Context, rt runtime.Runtime, containerID string, kind EventKind) (bool, error) {
	full, err := readLog(ctx, rt, containerID, 0)
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(full) {
		if ev, ok := DefaultPatterns.Match(strings.TrimRight(line, "\r\n")); ok && ev.Kind == kind {
			return true, nil
		}
	}
	return false, nil
}

// readLog reads and demuxes containerID's log. tail == 0 reads the whole log. Docker's
// stream carries an 8-byte multiplex header per frame (E5); stdcopy — the SDK's own
// demuxer, already a transitive dependency via internal/runtime — strips it rather than a
// hand-rolled parse of a format this package does not own.
func readLog(ctx context.Context, rt runtime.Runtime, containerID string, tail int) (string, error) {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Tail: tail})
	if err != nil {
		return "", fmt.Errorf("read logs of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()

	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		return "", fmt.Errorf("demux logs of container %s: %w", containerID, err)
	}
	return buf.String(), nil
}
