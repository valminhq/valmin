package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// RunningInDocker checks the container independently of the stored instance state.
func RunningInDocker(ctx context.Context, rt runtime.Runtime, inst *store.Instance) (bool, error) {
	if inst.ContainerID == nil {
		return false, nil
	}
	c, err := rt.Inspect(ctx, *inst.ContainerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether instance %s is running: %w", inst.ID, err)
	}
	return c.Running, nil
}

// AssertStopped refuses filesystem changes while Docker still has the server running.
func AssertStopped(ctx context.Context, rt runtime.Runtime, inst *store.Instance) error {
	running, err := RunningInDocker(ctx, rt, inst)
	if err != nil {
		return err
	}
	if running {
		return ErrServerRunning
	}
	return nil
}
