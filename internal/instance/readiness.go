package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/pkg/stdcopy"

	"github.com/valminhq/valmin/internal/runtime"
)

// readinessPollInterval paces AwaitReady's polling of the log and the container's running state.
// A plain poll rather than a live follow, since a job needs only a yes/no answer over
// jobs.ready_settle and ready_timeout.
const readinessPollInterval = 500 * time.Millisecond

// AwaitReady is 12 §3.3's readiness wait. confirmed is true when the anchored line was seen and
// false on the fallback path, where the container is still running and ready_settle elapsed
// without it. The fallback is a warning, not a failure (ADR-043), so a false is not an error.
// err is set only when the container exits before either path resolves, the readiness deadline
// passes, or ctx is done.
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
		c, err := rt.Inspect(ctx, containerID)
		if err != nil {
			return false, fmt.Errorf("inspect container %s: %w", containerID, err)
		}

		seen, err := containerLogMatches(ctx, rt, containerID, EventReady, bootOf(&c))
		if err != nil {
			return false, err
		}
		if seen {
			return true, nil
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

// SawSaveLine reports whether this boot's log contains the save-complete literal, checked once
// after the container has exited (12 §3.4) and never while it might still be writing the line.
//
// Scoped to this boot: the log survives every restart, so an unscoped search would find a
// previous stop's line and report a clean shutdown for one that wrote none (B2).
func SawSaveLine(ctx context.Context, rt runtime.Runtime, containerID string) (bool, error) {
	c, err := rt.Inspect(ctx, containerID)
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	return containerLogMatches(ctx, rt, containerID, EventSaveComplete, bootOf(&c))
}

// bootStartMargin is subtracted from a container's StartedAt when scoping a log read to the
// current boot: the clock stamping each line and the one behind StartedAt need not agree to the
// nanosecond, and being late by a millisecond drops the boot's first lines. It is far shorter
// than any restart, a stop alone taking seconds (03 §3.2.1). A var so a test can shrink it.
var bootStartMargin = time.Second

func bootOf(c *runtime.Container) time.Time {
	if c.StartedAt.IsZero() {
		return time.Time{}
	}
	return c.StartedAt.Add(-bootStartMargin)
}

// LogTail returns containerID's last n demuxed log lines, for attaching to a failed job's
// log (12 §7's "last N log lines attached").
func LogTail(ctx context.Context, rt runtime.Runtime, containerID string, n int) (string, error) {
	// Deliberately unscoped: a failed start's tail is for a human to read, and the lines
	// before this boot are context, not a false positive.
	return readLog(ctx, rt, containerID, n, time.Time{})
}

// containerLogMatches reports whether containerID's log carries a line of the given kind
// at or after since, matched through 14 §4.5's one pattern set rather than a literal of
// this file's own.
func containerLogMatches(
	ctx context.Context, rt runtime.Runtime, containerID string, kind EventKind, since time.Time,
) (bool, error) {
	full, err := readLog(ctx, rt, containerID, 0, since)
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

// readLog reads and demuxes containerID's log; tail == 0 reads all of it. Docker's stream carries
// an 8-byte multiplex header per frame (E5), stripped by the SDK's own stdcopy rather than a
// hand-rolled parse.
func readLog(
	ctx context.Context, rt runtime.Runtime, containerID string, tail int, since time.Time,
) (string, error) {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Tail: tail, Since: since})
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

// pluginLoadPollInterval paces AwaitPluginLoad, matching AwaitReady's own polling: a job
// needs a yes/no answer a handful of times over a short window, not a live follow.
const pluginLoadPollInterval = readinessPollInterval

// AwaitPluginLoad reports whether a modded server's BepInEx chainloader announced its plugin
// count within window (E1, 03 §5.2). A misconfigured chainloader boots cleanly, logs no error
// and loads zero mods, so an absent line is the only evidence there is.
//
// A false answer is a warning and never a failure: the function returns no error for "not seen",
// as ADR-043 does for the readiness line.
func AwaitPluginLoad(ctx context.Context, rt runtime.Runtime, containerID string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	ticker := time.NewTicker(pluginLoadPollInterval)
	defer ticker.Stop()

	for {
		c, err := rt.Inspect(ctx, containerID)
		if err == nil {
			// The one shared pattern set (E9), no second literal. Scoped to this boot, since the
			// log survives every restart and an unscoped search would keep finding the first modded
			// boot's line, so the assertion could never fire again.
			seen, matchErr := containerLogMatches(ctx, rt, containerID, EventPluginCount, bootOf(&c))
			if matchErr == nil && seen {
				return true
			}
			err = matchErr
		}
		if err != nil {
			// A busy daemon or a truncated stream is not evidence that plugins failed to
			// load. Log it and keep trying inside the window rather than warning an
			// operator about a healthy server.
			slog.WarnContext(ctx, "could not read the log while checking for a plugin count",
				slog.String("container_id", containerID), slog.Any("error", err))
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
