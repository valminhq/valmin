package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const lifecycleLogTailLines = 50

// Starter builds and starts the container described by the latest instance row.
type Starter struct {
	DB               *store.DB
	Runtime          runtime.Runtime
	Keeper           *crypto.Keeper
	HostRoot         string
	Image            string
	Network          string
	StopTimeout      time.Duration
	ReadySettle      time.Duration
	ReadyTimeout     time.Duration
	PluginLoadWindow time.Duration
}

func (s *Starter) Run(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 20, "starting container")
		return s.StartAndAwaitReady(ctx, jh, instanceID, containerID)
	}
}

// StartAndAwaitReady is shared by start and restart after the state enters starting.
func (s *Starter) StartAndAwaitReady(
	ctx context.Context,
	jh *jobs.Handle,
	instanceID, containerID string,
) jobs.Outcome {
	containerID, err := s.RebuildIfDrifted(ctx, jh, instanceID, containerID)
	if err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: err.Error(), OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}
	if err := s.Runtime.Start(ctx, containerID); err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: fmt.Sprintf("start container: %v", err), OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}
	jh.Progress(ctx, 60, "waiting for readiness")
	confirmed, err := instance.AwaitReady(ctx, s.Runtime, containerID, s.ReadySettle, s.ReadyTimeout)
	if err != nil {
		if tail, tailErr := instance.LogTail(ctx, s.Runtime, containerID, lifecycleLogTailLines); tailErr == nil {
			jh.Log(tail)
		}
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error:    fmt.Sprintf("the server did not become ready: %v", err),
			OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}
	msg := "running"
	if !confirmed {
		msg = "running (registration unconfirmed)"
	}
	if !s.assertPluginsLoaded(ctx, jh, instanceID, containerID) {
		msg += "; BepInEx did not report loading any plugins"
	}
	jh.Progress(ctx, 100, msg)
	return jobs.Outcome{
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return instance.FinishStartTx(ctx, tx, instanceID, instance.StateStarting, instance.StateRunning)
		},
	}
}

func (s *Starter) assertPluginsLoaded(ctx context.Context, jh *jobs.Handle, instanceID, containerID string) bool {
	inst, err := s.DB.InstanceByID(ctx, instanceID)
	if err != nil || inst == nil || !inst.Modded {
		return true
	}
	if instance.AwaitPluginLoad(ctx, s.Runtime, containerID, s.PluginLoadWindow) {
		return true
	}
	jh.Log("warning: this server is modded, but BepInEx never reported a plugin count. " +
		"The server is running and will keep running; it is probably running vanilla. " +
		"Check that BepInEx is installed under server/ and that [Logging.Console] Enabled is true.")
	slog.WarnContext(ctx, "modded instance started without a BepInEx plugin-count line",
		slog.String("instance_id", instanceID), slog.String("container_id", containerID))
	return false
}

// SpecFor builds the immutable container spec from the current row and stored password.
func (s *Starter) SpecFor(ctx context.Context, inst *store.Instance) (*runtime.ContainerSpec, error) {
	password, err := DecryptPassword(ctx, s.DB, s.Keeper, inst.ID)
	if err != nil {
		return nil, err
	}
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: inst.ID, DataDir: instance.DataDir(s.HostRoot, inst.ID), BasePort: inst.BasePort,
		ServerName: inst.ServerName, WorldName: inst.WorldName, Password: password,
		Public: inst.Public, Crossplay: inst.Crossplay, CrossplayInstanceID: inst.CrossplayInstanceID,
		Preset: deref(inst.Preset), Modifiers: deref(inst.Modifiers), ExtraArgs: deref(inst.ExtraArgs),
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
	}, s.Image, s.Network, s.StopTimeout)
	if err != nil {
		return nil, fmt.Errorf("build container spec for instance %s: %w", inst.ID, err)
	}
	return spec, nil
}

// EnsureInstanceContainer reuses a matching container after an interrupted creation.
func EnsureInstanceContainer(ctx context.Context, rt runtime.Runtime, spec *runtime.ContainerSpec) (string, error) {
	containers, err := rt.List(ctx, map[string]string{
		instance.LabelManaged: "true", instance.LabelInstanceID: spec.Labels[instance.LabelInstanceID],
	})
	if err != nil {
		return "", fmt.Errorf("find instance container: %w", err)
	}
	if len(containers) > 1 {
		return "", fmt.Errorf("multiple containers claim instance %s", spec.Labels[instance.LabelInstanceID])
	}
	if len(containers) == 1 {
		if containers[0].Labels[instance.LabelSpecHash] != spec.Labels[instance.LabelSpecHash] {
			return "", errors.New("existing instance container has a different immutable spec")
		}
		return containers[0].ID, nil
	}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create instance container: %w", err)
	}
	return id, nil
}

// RebuildIfDrifted replaces a container whose immutable spec differs from the current row.
func (s *Starter) RebuildIfDrifted(
	ctx context.Context,
	jh *jobs.Handle,
	instanceID, containerID string,
) (string, error) {
	inst, err := s.DB.InstanceByID(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("load instance %s: %w", instanceID, err)
	}
	if inst == nil {
		return "", fmt.Errorf("instance %s no longer exists", instanceID)
	}
	spec, err := s.SpecFor(ctx, inst)
	if err != nil {
		return "", err
	}
	live, err := s.Runtime.Inspect(ctx, containerID)
	switch {
	case err != nil && !errors.Is(err, runtime.ErrNotFound):
		return "", fmt.Errorf("inspect container %s: %w", containerID, err)
	case err == nil && live.Labels[instance.LabelSpecHash] == spec.Labels[instance.LabelSpecHash]:
		return containerID, nil
	}
	jh.Log("settings changed since this server was last created; rebuilding its container")
	if err := s.Runtime.Remove(ctx, containerID, false); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return "", fmt.Errorf("remove container %s: %w", containerID, err)
	}
	newID, err := s.Runtime.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("recreate container for instance %s: %w", instanceID, err)
	}
	if err := s.DB.SetInstanceContainerID(ctx, instanceID, newID); err != nil {
		return "", fmt.Errorf("repoint instance %s at its rebuilt container: %w", instanceID, err)
	}
	if err := jh.Checkpoint(ctx, "container_rebuilt"); err != nil {
		return "", fmt.Errorf("checkpoint rebuild of instance %s: %w", instanceID, err)
	}
	return newID, nil
}
