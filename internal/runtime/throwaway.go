package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// The labels every throwaway carries, so one the panel died beside is findable rather than
// invisible. Deliberately not 08 §1's io.valmin.managed and io.valmin.instance.id: those are
// what reconciliation joins Docker to the database on (08 §6.1), and a helper wearing an
// instance id would be mistaken for that instance's own container.
//
// They live here rather than beside the others in internal/instance because RunThrowaway is
// reached from internal/config as well, which cannot import that package.
const (
	// LabelThrowaway carries the purpose, so a leftover says what it was doing.
	LabelThrowaway = "io.valmin.throwaway"
	// LabelThrowawayKey carries the resource the container writes, where it has one. It is how
	// a surviving writer is found and removed before a second one is started against the same
	// path: a process-local lock cannot exclude a helper whose panel is gone.
	LabelThrowawayKey = "io.valmin.throwaway.key"
)

// removeTimeout bounds the cleanup. It runs on a context that is deliberately not cancelled, so
// without a deadline of its own a daemon that accepts and never answers blocks the caller for
// as long as it likes.
const removeTimeout = 30 * time.Second

// ThrowawaySpec is a one-shot container: created, run to completion, removed. It is the
// mechanism behind the host_data_root self-check (10 §1.2) and SteamCMD (08 §3.2).
type ThrowawaySpec struct {
	// Purpose names what this container is for, and is required: it becomes the label that
	// makes a leftover identifiable.
	Purpose string
	// Key names the resource this container writes, where it writes one. A throwaway that
	// touches nothing outside itself leaves it empty.
	Key string

	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	User       string
	Binds      []Bind
	// NoNetwork attaches the container to no network at all.
	NoNetwork bool
	// Stdout and Stderr receive the container's output as it is produced. A nil writer
	// discards that stream.
	Stdout io.Writer
	Stderr io.Writer
}

func (s *ThrowawaySpec) labels() map[string]string {
	labels := map[string]string{LabelThrowaway: s.Purpose}
	if s.Key != "" {
		labels[LabelThrowawayKey] = s.Key
	}
	return labels
}

// RunThrowaway runs spec to completion and returns its exit code. A non-zero exit is not an
// error; the caller decides what a failed run means. The container is removed even when the
// context is cancelled, so a timed-out self-check does not leave one behind.
func RunThrowaway(ctx context.Context, rt Runtime, spec *ThrowawaySpec) (int, error) {
	if spec.Purpose == "" {
		return 0, fmt.Errorf("throwaway %s names no Purpose: a leftover has to say what it was", spec.Image)
	}
	id, err := rt.Create(ctx, &ContainerSpec{
		Image:           spec.Image,
		Entrypoint:      spec.Entrypoint,
		Cmd:             spec.Cmd,
		Env:             spec.Env,
		User:            spec.User,
		Labels:          spec.labels(),
		Binds:           spec.Binds,
		NetworkDisabled: spec.NoNetwork,
	})
	if err != nil {
		return 0, fmt.Errorf("throwaway %s: %w", spec.Image, err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
		defer cancel()
		if err := rt.Remove(removeCtx, id, true); err != nil {
			slog.WarnContext(ctx, "throwaway container not removed",
				slog.String("container_id", id), slog.String("purpose", spec.Purpose),
				slog.Any("error", err))
		}
	}()

	if err := rt.Start(ctx, id); err != nil {
		return 0, fmt.Errorf("throwaway %s: %w", spec.Image, err)
	}

	rc, err := rt.Logs(ctx, id, LogOptions{Follow: true})
	if err != nil {
		return 0, fmt.Errorf("throwaway %s: %w", spec.Image, err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := stdcopy.StdCopy(writerOrDiscard(spec.Stdout), writerOrDiscard(spec.Stderr), rc); err != nil {
		return 0, fmt.Errorf("read output of throwaway %s: %w", spec.Image, err)
	}

	code, err := rt.Wait(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("throwaway %s: %w", spec.Image, err)
	}
	return code, nil
}

// RemoveThrowaways force-removes the throwaway containers this panel left behind, and reports
// how many. An empty key means all of them, which is the startup sweep; a key names one
// resource, which is how a caller makes sure nothing is still writing the path it is about to
// write itself.
//
// Force, because a survivor of a dead panel may still be running, and that is the case this
// exists for. It is safe at startup for the same reason a throwaway is a throwaway: nothing
// reads its result but the call that created it, and that call's process is gone.
func RemoveThrowaways(ctx context.Context, rt Runtime, key string) (int, error) {
	filter := map[string]string{LabelThrowaway: ""}
	if key != "" {
		filter[LabelThrowawayKey] = key
	}
	containers, err := rt.List(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("list throwaway containers: %w", err)
	}
	removed := 0
	for i := range containers {
		c := &containers[i]
		if err := rt.Remove(ctx, c.ID, true); err != nil {
			return removed, fmt.Errorf("remove throwaway container %s: %w", c.ID, err)
		}
		slog.InfoContext(ctx, "removed a leftover throwaway container",
			slog.String("container_id", c.ID),
			slog.String("purpose", c.Labels[LabelThrowaway]),
			slog.String("key", c.Labels[LabelThrowawayKey]))
		removed++
	}
	return removed, nil
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
