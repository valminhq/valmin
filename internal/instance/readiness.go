package instance

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// ContainerLogs reads containers' logs through the engine and matches them against one pattern
// set. Build it with NewContainerLogs.
type ContainerLogs struct {
	rt       runtime.Runtime
	patterns PatternSet
}

// NewContainerLogs returns a reader of rt's container logs that matches with patterns.
func NewContainerLogs(rt runtime.Runtime, patterns PatternSet) *ContainerLogs {
	return &ContainerLogs{rt: rt, patterns: patterns}
}

// CheckReady looks once at the log and the container, without waiting. confirmed is true when
// the current boot has logged its ready line; err is set when the container is not running.
func (l *ContainerLogs) CheckReady(ctx context.Context, containerID string) (confirmed bool, err error) {
	c, err := l.rt.Inspect(ctx, containerID)
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	seen, err := l.matches(ctx, containerID, readyEvent(&c), bootOf(&c))
	if err != nil {
		return false, err
	}
	if !c.Running {
		return false, fmt.Errorf("container exited with code %d before becoming ready", c.ExitCode)
	}
	return seen, nil
}

// ReadyLogged reports whether containerID's current boot has logged its ready line.
func (l *ContainerLogs) ReadyLogged(ctx context.Context, containerID string) (bool, error) {
	c, err := l.rt.Inspect(ctx, containerID)
	if err != nil {
		return false, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	return l.matches(ctx, containerID, readyEvent(&c), bootOf(&c))
}

// readyEvent is the line after which players can join c: the Steam backend connection, or for
// a crossplay server its PlayFab lobby. The flag is read from the container's own argv, so a
// -crossplay passed through extra_args counts too.
func readyEvent(c *runtime.Container) EventKind {
	if slices.Contains(c.Spec.Cmd, "-crossplay") {
		return EventCrossplayReady
	}
	return EventReady
}

// SawSaveLine reports whether the log carries the save-complete literal after since, checked
// once the container has exited (12 §3.4) and never while it might still be writing the line.
//
// `↯` since is the instant the stop was requested, not the boot. The server writes this literal
// on every save, autosaves included, so a boot-scoped search accepts an autosave from hours
// earlier as proof that this shutdown wrote the world — and the archive is then catalogued
// consistent over a world whose final save never completed (B2).
func (l *ContainerLogs) SawSaveLine(ctx context.Context, containerID string, since time.Time) (bool, error) {
	return l.matches(ctx, containerID, EventSaveComplete, since)
}

// bootStartMargin is subtracted from a container's StartedAt when scoping a log read to the
// current boot: the clock stamping each line and the one behind StartedAt need not agree to the
// nanosecond, and being late by a millisecond drops the boot's first lines. It is far shorter
// than any restart, a stop alone taking seconds.
const bootStartMargin = time.Second

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

// matches reports whether containerID's log carries a line of the given kind at or after
// since, matched through l's pattern set rather than a literal of this file's own.
//
// `↯` The stream is assembled per stream id by DemuxLines, exactly as the console reader does
// it, and never by concatenating both into one buffer. Docker frames are not lines (E5): a
// fragment of stdout followed by a fragment of stderr would otherwise join into a literal
// neither stream wrote, and an interleaved fragment would split a literal that one of them did.
//
// Lines are matched as they arrive and none is retained, so the cost of asking this question of
// a server that has been up for weeks is one line rather than the whole session.
func (l *ContainerLogs) matches(
	ctx context.Context, containerID string, kind EventKind, since time.Time,
) (bool, error) {
	rc, err := l.rt.Logs(ctx, containerID, runtime.LogOptions{Since: since})
	if err != nil {
		return false, fmt.Errorf("read logs of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()

	found := false
	if err := DemuxLines(rc, func(line Line) {
		if found {
			return
		}
		if ev, ok := l.patterns.Match(line.Text); ok && ev.Kind == kind {
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
