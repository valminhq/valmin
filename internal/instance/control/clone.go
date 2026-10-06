package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// CloneRun is the input retained while a clone job executes.
type CloneRun struct {
	Source, Destination                                    *store.Instance
	Password, ArchiveID, ArchivePath, RequestedBy, AuditIP string
}

// Cloner owns the source snapshot, file copy, and destination publication.
type Cloner struct {
	DB                       *store.DB
	Runtime                  runtime.Runtime
	Snapshotter              *Snapshotter
	HostRoot, Image, Network string
	StopTimeout              time.Duration
	ReadMods                 func(context.Context, *store.Instance) ([]store.InstanceMod, error)
}

func (c *Cloner) Run(run *CloneRun) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		return c.executeClone(ctx, jh, run)
	}
}

func (c *Cloner) executeClone(ctx context.Context, jh *jobs.Handle, run *CloneRun) jobs.Outcome {
	if err := instance.VerifyProcessUID(instance.WantCloneUID); err != nil {
		return cloneFailed(run.Destination.ID, err)
	}
	if err := c.reloadClone(ctx, run); err != nil {
		return cloneFailed(run.Destination.ID, err)
	}
	if out := cloneStep(ctx, jh, run.Destination.ID, 3, "creating directories", func() error {
		return instance.EnsureInstanceDirs(run.Destination.DataDir)
	}, "dirs_created"); out != nil {
		return *out
	}
	mods, out := c.cloneServerFiles(ctx, jh, run)
	if out != nil {
		return *out
	}
	archiveResult, worldPresent, out := c.cloneWorldFiles(ctx, jh, run)
	if out != nil {
		return *out
	}
	containerID, buildID, err := c.createCloneContainer(ctx, jh, run)
	if err != nil {
		return cloneFailed(run.Destination.ID, err)
	}
	if err := jh.Checkpoint(ctx, "container_created"); err != nil {
		return cloneFailed(run.Destination.ID, err)
	}

	jh.Progress(ctx, 100, "clone ready")
	return jobs.Outcome{
		Status:   jobs.StatusSucceeded,
		OnFinish: finishClone(run, mods, archiveResult, worldPresent, containerID, buildID),
	}
}

func (c *Cloner) reloadClone(ctx context.Context, run *CloneRun) error {
	source, err := c.DB.InstanceByID(ctx, run.Source.ID)
	if err != nil {
		return fmt.Errorf("reload clone source: %w", err)
	}
	if source == nil {
		return errors.New("clone source no longer exists")
	}
	destination, err := c.DB.InstanceByID(ctx, run.Destination.ID)
	if err != nil {
		return fmt.Errorf("reload clone destination: %w", err)
	}
	if destination == nil {
		return errors.New("clone destination no longer exists")
	}
	run.Source, run.Destination = source, destination
	return nil
}

func (c *Cloner) cloneServerFiles(
	ctx context.Context, jh *jobs.Handle, run *CloneRun,
) ([]store.InstanceMod, *jobs.Outcome) {
	jh.Progress(ctx, 8, "verifying source ownership")
	if err := instance.VerifyClonedOwnership(
		instance.ServerDir(run.Source.DataDir), instance.WantCloneUID); err != nil {
		out := cloneFailed(run.Destination.ID, err)
		return nil, &out
	}
	mods, err := c.ReadMods(ctx, run.Source)
	if err != nil {
		out := cloneFailed(run.Destination.ID, fmt.Errorf("read source mod manifest: %w", err))
		return nil, &out
	}
	for i := range mods {
		mods[i].InstanceID = run.Destination.ID
	}

	jh.Progress(ctx, 12, "copying server files")
	err = instance.CloneWithProgress(ctx,
		instance.ServerDir(run.Source.DataDir), instance.ServerDir(run.Destination.DataDir),
		clonePollInterval, func(pct int) {
			jh.Progress(ctx, 12+pct*38/100, "copying server files")
		})
	if err != nil {
		out := cloneFailed(run.Destination.ID, fmt.Errorf("copy server files: %w", err))
		return nil, &out
	}
	if err := instance.VerifyClonedOwnership(
		instance.ServerDir(run.Destination.DataDir), instance.WantCloneUID); err != nil {
		out := cloneFailed(run.Destination.ID, err)
		return nil, &out
	}
	// A disabled mod's files are beside server/, not in it, and its copied row says they are
	// parked (Q37); without them the clone could never enable it.
	if err := CloneParkedMods(run.Source.DataDir, run.Destination.DataDir); err != nil {
		out := cloneFailed(run.Destination.ID, fmt.Errorf("copy disabled mods: %w", err))
		return nil, &out
	}
	return mods, cloneCheckpoint(ctx, jh, run.Destination.ID, "server_cloned")
}

func (c *Cloner) cloneWorldFiles(
	ctx context.Context, jh *jobs.Handle, run *CloneRun,
) (backup.Result, bool, *jobs.Outcome) {
	jh.Progress(ctx, 55, "archiving source world")
	archiveResult, worldPresent, err := c.ArchiveWorld(ctx, run)
	if err != nil {
		out := cloneFailed(run.Destination.ID, err)
		return backup.Result{}, false, &out
	}
	if out := cloneCheckpoint(ctx, jh, run.Destination.ID, "world_archived"); out != nil {
		return backup.Result{}, false, out
	}

	jh.Progress(ctx, 70, "restoring destination world")
	if err := restoreCloneWorld(run.ArchivePath, instance.WorldsDir(run.Destination.DataDir)); err != nil {
		out := cloneFailed(run.Destination.ID, err)
		return backup.Result{}, false, &out
	}
	if !worldPresent {
		if err := os.Remove(run.ArchivePath); err != nil {
			out := cloneFailed(run.Destination.ID, fmt.Errorf("remove empty clone archive: %w", err))
			return backup.Result{}, false, &out
		}
	}
	return archiveResult, worldPresent, cloneCheckpoint(ctx, jh, run.Destination.ID, "world_restored")
}

func (c *Cloner) createCloneContainer(
	ctx context.Context, jh *jobs.Handle, run *CloneRun,
) (containerID, buildID string, err error) {
	buildID, err = instance.InstalledBuildID(run.Destination.DataDir)
	if err != nil {
		return "", "", fmt.Errorf("read cloned server build: %w", err)
	}
	run.Destination.GameBuildID = &buildID
	spec, err := c.cloneSpec(run)
	if err != nil {
		return "", "", err
	}
	jh.Progress(ctx, 90, "creating destination container")
	containerID, err = EnsureInstanceContainer(ctx, c.Runtime, spec)
	return containerID, buildID, err
}

func finishClone(
	run *CloneRun, mods []store.InstanceMod, archiveResult backup.Result, worldPresent bool,
	containerID, buildID string,
) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := instance.FinishProvisioningTx(ctx, tx, run.Destination.ID,
			instance.StateProvisioning, instance.StateStopped, containerID, buildID); err != nil {
			return fmt.Errorf("finish clone destination: %w", err)
		}
		if err := store.TxUpsertInstanceMods(ctx, tx, mods); err != nil {
			return fmt.Errorf("copy clone mod manifest: %w", err)
		}
		if !worldPresent {
			return nil
		}
		if err := store.TxCreateBackup(ctx, tx, &store.Backup{
			ID: run.ArchiveID, InstanceID: run.Destination.ID, Path: archiveResult.Path,
			SizeBytes: archiveResult.SizeBytes, SHA256: archiveResult.SHA256,
			WorldName: run.Source.WorldName, Trigger: store.TriggerManual, Consistent: true,
		}); err != nil {
			return fmt.Errorf("catalogue clone seed backup: %w", err)
		}
		return nil
	}
}

func cloneStep(
	ctx context.Context, jh *jobs.Handle, destinationID string, progress int, message string,
	work func() error, checkpoint string,
) *jobs.Outcome {
	jh.Progress(ctx, progress, message)
	if err := work(); err != nil {
		out := cloneFailed(destinationID, err)
		return &out
	}
	return cloneCheckpoint(ctx, jh, destinationID, checkpoint)
}

func cloneCheckpoint(
	ctx context.Context, jh *jobs.Handle, destinationID, checkpoint string,
) *jobs.Outcome {
	if err := jh.Checkpoint(ctx, checkpoint); err != nil {
		out := cloneFailed(destinationID, err)
		return &out
	}
	if jh.CancelRequested(ctx) {
		return &jobs.Outcome{Status: jobs.StatusCancelled, OnFinish: ProvisionOnFinishError(destinationID)}
	}
	return nil
}

func cloneFailed(destinationID string, err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: FailureCode(err).String(), Error: err.Error(),
		OnFinish: ProvisionOnFinishError(destinationID),
	}
}

// archiveCloneWorld archives the source's worlds/ to the clone's seed archive, which is
// catalogued as consistent, so the source must be down in Docker for the whole copy.
func (c *Cloner) ArchiveWorld(ctx context.Context, run *CloneRun) (backup.Result, bool, error) {
	present, err := cloneWorldPairPresent(run.Source)
	if err != nil {
		return backup.Result{}, false, err
	}
	res, err := c.Snapshotter.ArchiveStoppedWorlds(ctx, run.Source, run.ArchivePath)
	if err != nil {
		return backup.Result{}, false, fmt.Errorf("archive source world: %w", err)
	}
	if present {
		if _, err := backup.Verify(res.Path, run.Source.WorldName); err != nil {
			_ = os.Remove(res.Path)
			return backup.Result{}, false, fmt.Errorf("verify source world archive: %w", err)
		}
	}
	return res, present, nil
}

// cloneWorldPairPresent reports whether the source has a whole world to clone, in either of
// 03 §4's layouts (ADR-179). Half a world is an error rather than an absence: a source that
// lost one half is a source whose clone would be silently empty.
func cloneWorldPairPresent(inst *store.Instance) (bool, error) {
	scan, err := backup.ScanWorlds(instance.WorldsDir(inst.DataDir))
	if err != nil {
		return false, fmt.Errorf("inspect source world: %w", err)
	}
	world, present := scan[inst.WorldName]
	if !present {
		return false, nil
	}
	if !world.Complete() {
		return false, fmt.Errorf("source world %s is missing half of itself", inst.WorldName)
	}
	return true, nil
}

func restoreCloneWorld(archivePath, live string) error {
	if _, err := backup.RecoverSwap(live); err != nil {
		return fmt.Errorf("recover destination world swap: %w", err)
	}
	staged := live + backup.StagedSuffix
	if err := backup.DiscardStaged(live); err != nil {
		return fmt.Errorf("clear destination world staging: %w", err)
	}
	if err := backup.Extract(archivePath, "", staged); err != nil {
		return fmt.Errorf("extract source world archive: %w", err)
	}
	// A clone destination has no world before this, so its live directory is absent while the
	// extraction runs: without this claim a crash mid-extraction is indistinguishable from a
	// finished staging, and recovery would publish a truncated world (ADR-177).
	if err := backup.MarkStaged(live); err != nil {
		return fmt.Errorf("mark destination world staged: %w", err)
	}
	if err := backup.Swap(live); err != nil {
		return fmt.Errorf("publish destination world: %w", err)
	}
	return nil
}

func (c *Cloner) cloneSpec(run *CloneRun) (*runtime.ContainerSpec, error) {
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: run.Destination.ID, DataDir: instance.DataDir(c.HostRoot, run.Destination.ID),
		BasePort: run.Destination.BasePort, ServerName: run.Destination.ServerName,
		WorldName: run.Destination.WorldName, Password: run.Password,
		Public: run.Destination.Public, Crossplay: run.Destination.Crossplay,
		CrossplayInstanceID: run.Destination.CrossplayInstanceID,
		Preset:              deref(run.Destination.Preset), Modifiers: deref(run.Destination.Modifiers),
		ExtraArgs: deref(run.Destination.ExtraArgs), MemLimitMB: run.Destination.MemLimitMB,
		CPULimit: run.Destination.CPULimit,
	}, c.Image, c.Network, c.StopTimeout)
	if err != nil {
		return nil, fmt.Errorf("build destination container spec: %w", err)
	}
	return spec, nil
}

// cloneParkedMods copies the source's parking tree, if it has one, to the destination's.
func CloneParkedMods(sourceDataDir, destinationDataDir string) error {
	src, err := os.OpenRoot(instance.ParkedModsDir(sourceDataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open the parking tree: %w", err)
	}
	defer func() { _ = src.Close() }()
	if err := installer.CopyTree(src, instance.ParkedModsDir(destinationDataDir)); err != nil {
		return fmt.Errorf("copy the parking tree: %w", err)
	}
	return nil
}
