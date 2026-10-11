package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
		// The exit is checked before the line is accepted, not after. The ready line says the
		// server reached that point, never that it is still there, so a process that announces
		// itself and then dies would otherwise make a start succeed and publish `running`
		// (E6) — and a later observer pass correcting the row does not un-succeed the job.
		if !c.Running {
			return false, fmt.Errorf("container exited with code %d before becoming ready", c.ExitCode)
		}
		if seen {
			return true, nil
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

// SawSaveLine reports whether the log carries the save-complete literal after since, checked
// once the container has exited (12 §3.4) and never while it might still be writing the line.
//
// `↯` since is the instant the stop was requested, not the boot. The server writes this literal
// on every save, autosaves included, so a boot-scoped search accepts an autosave from hours
// earlier as proof that this shutdown wrote the world — and the archive is then catalogued
// consistent over a world whose final save never completed (B2).
func SawSaveLine(
	ctx context.Context, rt runtime.Runtime, containerID string, since time.Time,
) (bool, error) {
	return containerLogMatches(ctx, rt, containerID, EventSaveComplete, since)
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
//
// `↯` The stream is assembled per stream id by DemuxLines, exactly as the console reader does
// it, and never by concatenating both into one buffer. Docker frames are not lines (E5): a
// fragment of stdout followed by a fragment of stderr would otherwise join into a literal
// neither stream wrote, and an interleaved fragment would split a literal that one of them did.
//
// Lines are matched as they arrive and none is retained, so the cost of asking this question of
// a server that has been up for weeks is one line rather than the whole session.
func containerLogMatches(
	ctx context.Context, rt runtime.Runtime, containerID string, kind EventKind, since time.Time,
) (bool, error) {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Since: since})
	if err != nil {
		return false, fmt.Errorf("read logs of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()

	found := false
	if err := DemuxLines(rc, func(l Line) {
		if found {
			return
		}
		if ev, ok := ActivePatterns().Match(l.Text); ok && ev.Kind == kind {
			found = true
		}
	}); err != nil {
		return false, err
	}
	return found, nil
}

// readLog reads containerID's log as whole lines; tail == 0 reads all of it. Assembly is
// per stream id (E5), so a line is a line on the stream that wrote it and never a splice of
// both — the same discipline the console reader applies, for the same reason.
func readLog(
	ctx context.Context, rt runtime.Runtime, containerID string, tail int, since time.Time,
) (string, error) {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Tail: tail, Since: since})
	if err != nil {
		return "", fmt.Errorf("read logs of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()

	var out strings.Builder
	if err := DemuxLines(rc, func(l Line) {
		out.WriteString(l.Text)
		out.WriteByte('\n')
	}); err != nil {
		return "", err
	}
	return out.String(), nil
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
