package control

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Provisioner owns the resumable phases of instance creation.
type Provisioner struct {
	DB                                                *store.DB
	Engine                                            *jobs.Engine
	Runtime                                           runtime.Runtime
	DataRoot, HostRoot, SteamCMDImage, Image, Network string
	StopTimeout                                       time.Duration
	AdvanceChain                                      func(context.Context, string)
}

// ProvisionRun is everything the provision job's runner needs, carried as one value.
type ProvisionRun struct {
	BuildID             string
	InstanceID          string
	Name                string
	BasePort            int
	DataDir             string
	ServerName          string
	WorldName           string
	Password            string
	Public              bool
	Crossplay           bool
	CrossplayInstanceID string
	Preset              string
	Modifiers           string
	ExtraArgs           string
	MemLimitMB          int
	CPULimit            *float64
	StartAfterProvision bool
	// RequestedBy is the user id to attribute this run to, or "" for a run the panel
	// started on its own — 12 §9.2's resume after a crash has no user behind it.
	RequestedBy string
	// Audit is the trail entry the claim writes. Nil for a resumed run, which repeats a request
	// already on record.
	Audit *store.AuditEntry
}

// provisionRunFor builds the provision run that installs an existing instance again.
func provisionRunFor(inst *store.Instance, password string, startAfter bool) *ProvisionRun {
	return &ProvisionRun{
		InstanceID: inst.ID, Name: inst.Name, BasePort: inst.BasePort, DataDir: inst.DataDir,
		ServerName: inst.ServerName, WorldName: inst.WorldName, Password: password,
		Public: inst.Public, Crossplay: inst.Crossplay, CrossplayInstanceID: inst.CrossplayInstanceID,
		Preset: deref(inst.Preset), Modifiers: deref(inst.Modifiers), ExtraArgs: deref(inst.ExtraArgs),
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
		StartAfterProvision: startAfter,
	}
}

// clonePollInterval is how often CloneWithProgress samples the destination's size during a
// full (non-reflink) copy. Two seconds matches jobs.progress_interval's own throttle — no
// point polling faster than the row that reports it is allowed to change.
const clonePollInterval = 2 * time.Second

// Run is the provision job's runner, holding no transaction. Every phase
// is idempotent, so a from-scratch re-run after a crash converges; the checkpoint written after
// each phase is what a resume keys off.
func (p *Provisioner) Run(run *ProvisionRun) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		if outcome, stop := p.provisionDirs(ctx, jh, run); stop {
			return outcome
		}
		if outcome, stop := p.provisionBuildCache(ctx, jh, run); stop {
			return outcome
		}
		if outcome, stop := p.provisionClone(ctx, jh, run); stop {
			return outcome
		}
		return p.provisionCreateContainer(ctx, jh, run)
	}
}

func (p *Provisioner) provisionDirs(ctx context.Context, jh *jobs.Handle, run *ProvisionRun) (jobs.Outcome, bool) {
	jh.Progress(ctx, 2, "creating directories")
	if err := instance.EnsureInstanceDirs(run.DataDir); err != nil {
		return provisionFailed(run.InstanceID, fmt.Errorf("create instance directories: %w", err)), true
	}
	return provisionCheckpoint(ctx, jh, run.InstanceID, "dirs_created")
}

func (p *Provisioner) provisionBuildCache(
	ctx context.Context,
	jh *jobs.Handle,
	run *ProvisionRun,
) (jobs.Outcome, bool) {
	jh.Progress(ctx, 10, "downloading game files")
	id, err := instance.CachePublicBuild(ctx, &instance.BuildCacheInput{
		Runtime:      p.Runtime,
		Image:        p.SteamCMDImage,
		HostCacheDir: instance.CacheDir(p.HostRoot),
		HostDataRoot: p.HostRoot,
		CacheDir:     instance.CacheDir(p.DataRoot),
		// A retry that says nothing reads as a hang: the download is the longest phase of
		// the longest job in the panel, and Q31's failure lands in the first seconds of it.
		Report: func(attempt, of int, err error) {
			jh.Log(fmt.Sprintf("steamcmd attempt %d of %d failed (%v); retrying", attempt, of, err))
			jh.Progress(ctx, 10, fmt.Sprintf("retrying download (attempt %d of %d)", attempt+1, of))
		},
	})
	if err != nil {
		return provisionFailed(run.InstanceID, fmt.Errorf("build cache: %w", err)), true
	}
	run.BuildID = id
	return provisionCheckpoint(ctx, jh, run.InstanceID, "build_cached")
}

func (p *Provisioner) provisionClone(ctx context.Context, jh *jobs.Handle, run *ProvisionRun) (jobs.Outcome, bool) {
	var fsType string
	_, _ = p.DB.KVGet(ctx, instance.DataFSTypeKey, &fsType) // "" (unknown) degrades to the safe, slow-path budget
	cloneStart, cloneEnd := instance.CloneProgressBudget(fsType)
	jh.Progress(ctx, cloneStart, "cloning game files")

	srcDir := instance.CacheDir(p.DataRoot) + "/" + run.BuildID
	dstDir := run.DataDir + "/server"
	err := instance.CloneWithProgress(ctx, srcDir, dstDir, clonePollInterval, func(pct int) {
		jh.Progress(ctx, cloneStart+(cloneEnd-cloneStart)*pct/100, "cloning game files")
	})
	if err != nil {
		return provisionFailed(run.InstanceID, fmt.Errorf("clone game files: %w", err)), true
	}
	if err := instance.VerifyClonedOwnership(dstDir, instance.WantCloneUID); err != nil {
		return provisionFailed(run.InstanceID, err), true
	}
	run.BuildID, err = instance.InstalledBuildID(run.DataDir)
	if err != nil {
		return provisionFailed(run.InstanceID, err), true
	}
	return provisionCheckpoint(ctx, jh, run.InstanceID, "cloned")
}

func (p *Provisioner) provisionCreateContainer(ctx context.Context, jh *jobs.Handle, run *ProvisionRun) jobs.Outcome {
	jh.Progress(ctx, 90, "creating container")
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID:          run.InstanceID,
		DataDir:             instance.DataDir(p.HostRoot, run.InstanceID),
		BasePort:            run.BasePort,
		ServerName:          run.ServerName,
		WorldName:           run.WorldName,
		Password:            run.Password,
		Public:              run.Public,
		Crossplay:           run.Crossplay,
		CrossplayInstanceID: run.CrossplayInstanceID,
		Preset:              run.Preset,
		Modifiers:           run.Modifiers,
		ExtraArgs:           run.ExtraArgs,
		MemLimitMB:          run.MemLimitMB,
		CPULimit:            run.CPULimit,
	}, p.Image, p.Network, p.StopTimeout)
	if err != nil {
		return provisionFailed(run.InstanceID, fmt.Errorf("build container spec: %w", err))
	}
	containerID, err := ensureInstanceContainer(ctx, p.Runtime, spec)
	if err != nil {
		return provisionFailed(run.InstanceID, fmt.Errorf("create container: %w", err))
	}
	// Past this checkpoint the job is no longer cancellable (ProvisionCancelPolicy): a
	// container now exists, so nothing after this point is discardable for free.
	if err := jh.Checkpoint(ctx, "container_created"); err != nil {
		return provisionFailed(run.InstanceID, err)
	}

	jh.Progress(ctx, 100, "provisioned")
	return jobs.Outcome{
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			if err := instance.FinishProvisioningTx(ctx, tx, run.InstanceID,
				instance.StateProvisioning, instance.StateStopped,
				containerID, run.BuildID); err != nil {
				return fmt.Errorf("finish provisioning instance %s: %w", run.InstanceID, err)
			}
			return nil
		},
		AfterFinish: func(ctx context.Context) { p.AdvanceChain(ctx, run.InstanceID) },
	}
}

// provisionCheckpoint writes checkpoint and reports whether the runner must stop here:
// either the write itself failed, or a cancel was requested while still within
// ProvisionCancelPolicy's cancellable range.
func provisionCheckpoint(ctx context.Context, jh *jobs.Handle, instanceID, checkpoint string) (jobs.Outcome, bool) {
	if err := jh.Checkpoint(ctx, checkpoint); err != nil {
		return provisionFailed(instanceID, err), true
	}
	if jh.CancelRequested(ctx) {
		return jobs.Outcome{Status: jobs.StatusCancelled, OnFinish: provisionOnFinishError(instanceID)}, true
	}
	return jobs.Outcome{}, false
}

func provisionFailed(instanceID string, err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error(),
		OnFinish: provisionOnFinishError(instanceID),
	}
}

// provisionOnFinishError is the failed and cancelled paths' shared OnFinish (12 §8). Partial
// artefacts are left in place: the directories, the cache entry and a half-cloned server/ are
// removed by an explicit delete job, never implicitly here.
func provisionOnFinishError(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := instance.SetStateTx(
			ctx, tx, instanceID, instance.StateProvisioning, instance.StateError); err != nil {
			return fmt.Errorf("park instance %s in error: %w", instanceID, err)
		}
		return nil
	}
}
