package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// FailureCode identifies a failed stop check in job outcomes.
func FailureCode(err error) errcode.Code {
	if errors.Is(err, instance.ErrServerRunning) {
		return errcode.InstanceMustBeStopped
	}
	return errcode.Internal
}

// RunWorldDelete archives the savedir before removing a named world.
func (h *Snapshotter) RunWorldDelete(inst *store.Instance, world *instance.World) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 25, "backing up the worlds already there")
		snapshot, err := h.Snapshot(ctx, inst, store.TriggerPreImport)
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: FailureCode(err).String(),
				Error: fmt.Sprintf("could not back up the existing world: %v", err),
			}
		}
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, 75, "removing "+world.Name)
		if err := instance.RemoveWorld(inst.DataDir, world); err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
				Error: fmt.Sprintf("could not remove the world: %v", err),
			}
		}
		msg := world.Name + " removed"
		if world.Name == inst.WorldName {
			msg += "; the server will generate a new world on its next start"
		}
		jh.Progress(ctx, 100, msg)
		return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: snapshot}
	}
}

// RunWorldImport validates and publishes a staged world after a stopped-world snapshot.
func (h *Snapshotter) RunWorldImport(inst *store.Instance, staging string, allowVariant bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(staging) }()
		jh.Progress(ctx, 10, "validating the upload")
		world, violations := instance.ValidateImport(staging, allowVariant)
		if len(violations) > 0 {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: errcode.ValidationFailed.String(),
				Error: violations[0].Error(),
			}
		}
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, 35, "backing up the world already there")
		snapshot, err := h.Snapshot(ctx, inst, store.TriggerPreImport)
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: FailureCode(err).String(),
				Error: fmt.Sprintf("could not back up the existing world: %v", err),
			}
		}
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, 75, "installing the world")
		if err := h.InstallWorld(ctx, inst, world, staging); err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: FailureCode(err).String(),
				Error: fmt.Sprintf("could not install the world: %v", err),
			}
		}
		msg := "world imported"
		if world.Info.Name != inst.WorldName {
			msg = fmt.Sprintf("world imported (its internal name is %q, the instance loads %q)",
				world.Info.Name, inst.WorldName)
		}
		jh.Progress(ctx, 100, msg)
		return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: snapshot}
	}
}

// InstallWorld publishes a validated world under the instance's configured name.
func (h *Snapshotter) InstallWorld(
	ctx context.Context, inst *store.Instance, world *instance.UploadedWorld, staging string,
) error {
	files := world.Install(staging, inst.WorldName)
	if !world.Directory {
		for _, f := range files {
			rel := filepath.Join(instance.WorldsLocalDir, f.Name)
			if err := installStagedWorldFile(inst.DataDir, rel, f.Path); err != nil {
				return fmt.Errorf("install %s: %w", rel, err)
			}
		}
		return nil
	}
	live := filepath.Join(instance.WorldsDir(inst.DataDir), instance.WorldsLocalDir, inst.WorldName)
	if err := backup.DiscardStaged(live); err != nil {
		return fmt.Errorf("clear a previous staging: %w", err)
	}
	staged := inst.WorldName + backup.StagedSuffix
	for _, f := range files {
		rel := filepath.Join(instance.WorldsLocalDir, staged, filepath.Base(f.Name))
		if err := installStagedWorldFile(inst.DataDir, rel, f.Path); err != nil {
			_ = backup.DiscardStaged(live)
			return fmt.Errorf("install %s: %w", rel, err)
		}
	}
	if err := backup.MarkStaged(live); err != nil {
		_ = backup.DiscardStaged(live)
		return fmt.Errorf("mark the staged world complete: %w", err)
	}
	if err := AssertStopped(ctx, h.Runtime, inst); err != nil {
		_ = backup.DiscardStaged(live)
		return err
	}
	if err := backup.Swap(live); err != nil {
		return fmt.Errorf("publish the imported world: %w", err)
	}
	return nil
}

func installStagedWorldFile(dataDir, name, src string) error {
	in, err := os.Open(src) //nolint:gosec // src is validated against the panel's staging tree
	if err != nil {
		return fmt.Errorf("open staged file: %w", err)
	}
	defer func() { _ = in.Close() }()
	if err := instance.WriteWorldFileFromReader(dataDir, name, in); err != nil {
		return fmt.Errorf("write staged file: %w", err)
	}
	return nil
}
