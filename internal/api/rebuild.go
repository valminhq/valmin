package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// specFor builds the container spec an instance's row describes, decrypting its game
// password to do so (10 §3).
func (h *Instances) specFor(ctx context.Context, inst *store.Instance) (*runtime.ContainerSpec, error) {
	password, err := h.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.Location{Table: "instances", Column: "password", RowID: inst.ID},
		mustReadPassword(ctx, h.DB, inst.ID),
	)
	if err != nil {
		return nil, fmt.Errorf("decrypt password for instance %s: %w", inst.ID, err)
	}
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: inst.ID, DataDir: inst.DataDir, BasePort: inst.BasePort,
		ServerName: inst.ServerName, WorldName: inst.WorldName, Password: string(password),
		Public: inst.Public, Crossplay: inst.Crossplay, CrossplayInstanceID: inst.CrossplayInstanceID,
		Preset: deref(inst.Preset), Modifiers: deref(inst.Modifiers), ExtraArgs: deref(inst.ExtraArgs),
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
	}, h.Cfg.Game.Image, h.Cfg.Game.StopTimeout.Std())
	if err != nil {
		return nil, fmt.Errorf("build container spec for instance %s: %w", inst.ID, err)
	}
	return spec, nil
}

// rebuildIfDrifted returns the container the caller should start, recreating it first when
// the instance row no longer describes the one it is pointed at. Drift is a mismatch between
// the row's spec hash and the one on the live container; an unchanged instance keeps its
// container.
//
// A container's Cmd, binds and host config are fixed at creation, so an edited launch field
// or resource limit reaches Docker only through a new container. The recreation goes through
// instance.BuildSpec so that every set-once rule in INVARIANTS.md §A is applied by the same
// code that applies it at provision.
//
// The caller holds the instance lock, which is what keeps the observer from writing to the
// row while the container is briefly absent (C14).
func (h *Instances) rebuildIfDrifted(
	ctx context.Context, jh *jobs.Handle, instanceID, containerID string,
) (string, error) {
	inst, err := h.DB.InstanceByID(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("load instance %s: %w", instanceID, err)
	}
	if inst == nil {
		return "", fmt.Errorf("instance %s no longer exists", instanceID)
	}

	spec, err := h.specFor(ctx, inst)
	if err != nil {
		return "", err
	}

	live, err := h.Runtime.Inspect(ctx, containerID)
	switch {
	case err != nil && !errors.Is(err, runtime.ErrNotFound):
		return "", fmt.Errorf("inspect container %s: %w", containerID, err)
	case err == nil && live.Labels[instance.LabelSpecHash] == spec.Labels[instance.LabelSpecHash]:
		return containerID, nil
	}

	// Remove precedes Create because the old container holds the name and the published UDP
	// pair the new one needs. A failure between them therefore leaves the instance with no
	// container: the caller parks it in `error`, where 12 §9.2 already resolves a row whose
	// container is missing. container_id is left pointing at the removed container rather
	// than cleared, since the label join finds the truth without it (08 §6.1).
	jh.Log("settings changed since this server was last created; rebuilding its container")
	if err := h.Runtime.Remove(ctx, containerID, false); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return "", fmt.Errorf("remove container %s: %w", containerID, err)
	}
	newID, err := h.Runtime.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("recreate container for instance %s: %w", instanceID, err)
	}
	if err := h.DB.SetInstanceContainerID(ctx, instanceID, newID); err != nil {
		return "", fmt.Errorf("repoint instance %s at its rebuilt container: %w", instanceID, err)
	}
	if err := jh.Checkpoint(ctx, "container_rebuilt"); err != nil {
		return "", fmt.Errorf("checkpoint rebuild of instance %s: %w", instanceID, err)
	}
	return newID, nil
}
