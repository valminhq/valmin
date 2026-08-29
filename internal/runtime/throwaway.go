package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/docker/docker/pkg/stdcopy"
)

// ThrowawaySpec is a one-shot container: created, run to completion, removed. It is the
// mechanism behind the host_data_root self-check (10 §1.2) and SteamCMD (08 §3.2).
type ThrowawaySpec struct {
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

// RunThrowaway runs spec to completion and returns its exit code. A non-zero exit is not
// an error: the caller decides what a failed run means.
//
// The container is removed even when the context is cancelled, so a timed-out self-check
// does not leave one behind.
func RunThrowaway(ctx context.Context, rt Runtime, spec *ThrowawaySpec) (int, error) {
	id, err := rt.Create(ctx, &ContainerSpec{
		Image:           spec.Image,
		Entrypoint:      spec.Entrypoint,
		Cmd:             spec.Cmd,
		Env:             spec.Env,
		User:            spec.User,
		Binds:           spec.Binds,
		NetworkDisabled: spec.NoNetwork,
	})
	if err != nil {
		return 0, fmt.Errorf("throwaway %s: %w", spec.Image, err)
	}
	defer func() {
		if err := rt.Remove(context.WithoutCancel(ctx), id, true); err != nil {
			slog.WarnContext(ctx, "throwaway container not removed",
				slog.String("container_id", id), slog.Any("error", err))
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

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
