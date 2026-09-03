package instance

import (
	"context"
	"errors"
	"fmt"

	"github.com/valminhq/valmin/internal/runtime"
)

// Reconcile is what `POST /instances/{id}/acknowledge` runs for one instance (12 §2.4): the
// same question the observer answers at startup for every instance, asked here
// on demand for one that a human is looking at. It does not run the full crash-recovery
// matrix of 12 §9.2 — that matrix resolves a *transient* state after a crash; acknowledge
// only ever fires from the durable `error` state, where the only question left is whether a
// container is currently running.
//
// containerID empty, or the container gone, reconciles to stopped — never provisioning
// began but was never finished by a container this call can find. A running container
// reconciles to running; Docker wins over whatever the row last said (08 §6.1).
func Reconcile(ctx context.Context, rt runtime.Runtime, containerID string) (State, error) {
	if containerID == "" {
		return StateStopped, nil
	}
	c, err := rt.Inspect(ctx, containerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return StateStopped, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	if c.Running {
		return StateRunning, nil
	}
	return StateStopped, nil
}
