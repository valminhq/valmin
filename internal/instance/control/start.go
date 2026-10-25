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
	Engine           *jobs.Engine
	Runtime          runtime.Runtime
	Streams          *instance.Streams
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
		return s.startAndAwaitReady(ctx, jh, instanceID, containerID)
	}
}

// startAndAwaitReady is shared by start and restart after the state enters starting.
func (s *Starter) startAndAwaitReady(
	ctx context.Context,
	jh *jobs.Handle,
	instanceID, containerID string,
) jobs.Outcome {
	containerID, err := s.rebuildIfDrifted(ctx, jh, instanceID, containerID)
	if err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: err.Error(), OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}
	settleConfigs(ctx, s.DB, jh, instanceID)
	if err := s.Runtime.Start(ctx, containerID); err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: fmt.Sprintf("start container: %v", err), OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}
	jh.Progress(ctx, 60, "waiting for readiness")
	confirmed, err := s.Streams.AwaitReady(ctx, containerID, s.ReadySettle)
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
	if s.Streams.AwaitPluginLoad(ctx, containerID, s.PluginLoadWindow) {
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
	spec, err := instance.BuildSpec(launchSpecFor(inst, s.HostRoot, password), s.Image, s.Network, s.StopTimeout)
	if err != nil {
		return nil, fmt.Errorf("build container spec for instance %s: %w", inst.ID, err)
	}
	return spec, nil
}

// launchSpecFor is the one mapping from an instance row to its launch spec, which provision,
// clone and start all build the container from, so their spec hashes agree.
func launchSpecFor(inst *store.Instance, hostRoot, password string) *instance.LaunchSpec {
	return &instance.LaunchSpec{
		InstanceID: inst.ID, DataDir: instance.DataDir(hostRoot, inst.ID), BasePort: inst.BasePort,
		ServerName: inst.ServerName, WorldName: inst.WorldName, Password: password,
		Public: inst.Public, Crossplay: inst.Crossplay, CrossplayInstanceID: inst.CrossplayInstanceID,
		Preset: deref(inst.Preset), Modifiers: deref(inst.Modifiers), ExtraArgs: deref(inst.ExtraArgs),
		Backups: inst.GameBackups, MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
	}
}

// instanceContainers lists the panel's containers labelled for spec's instance, found by label
// rather than by a stored id, which an interrupted creation may never have recorded.
func instanceContainers(
	ctx context.Context,
	rt runtime.Runtime,
	spec *runtime.ContainerSpec,
) ([]runtime.Container, error) {
	containers, err := rt.List(ctx, map[string]string{
		instance.LabelManaged: "true", instance.LabelInstanceID: spec.Labels[instance.LabelInstanceID],
	})
	if err != nil {
		return nil, fmt.Errorf("find instance container: %w", err)
	}
	return containers, nil
}

// ensureInstanceContainer reuses a matching container after an interrupted creation.
func ensureInstanceContainer(ctx context.Context, rt runtime.Runtime, spec *runtime.ContainerSpec) (string, error) {
	containers, err := instanceContainers(ctx, rt, spec)
	if err != nil {
		return "", err
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

// replaceContainer leaves the instance one container built from spec: it reuses one an
// interrupted rebuild already created, removes every other labelled container, and creates one
// if none matched.
func (s *Starter) replaceContainer(ctx context.Context, spec *runtime.ContainerSpec) (string, error) {
	containers, err := instanceContainers(ctx, s.Runtime, spec)
	if err != nil {
		return "", err
	}
	keep := ""
	for i := range containers {
		c := &containers[i]
		if keep == "" && c.Labels[instance.LabelSpecHash] == spec.Labels[instance.LabelSpecHash] {
			keep = c.ID
			continue
		}
		if err := s.Runtime.Remove(ctx, c.ID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return "", fmt.Errorf("remove container %s: %w", c.ID, err)
		}
	}
	if keep != "" {
		return keep, nil
	}
	id, err := s.Runtime.Create(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("create instance container: %w", err)
	}
	return id, nil
}

// rebuildIfDrifted replaces a container whose immutable spec differs from the current row.
func (s *Starter) rebuildIfDrifted(
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
	newID, err := s.replaceContainer(ctx, spec)
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
