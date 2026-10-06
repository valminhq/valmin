package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	UpdateBuildCached  = "build_cached"
	UpdateCloned       = "cloned"
	UpdateModsReplayed = "mods_replayed"
	UpdateSwapStarted  = "swap_started"
)

var ErrModdedNotConfirmed = errors.New("modded instance was not confirmed for a game update")

func ConfirmModded(inst *store.Instance, confirmed bool) error {
	if confirmed {
		return nil
	}
	modded, err := instance.HasDoorstop(inst.DataDir)
	if err != nil {
		return fmt.Errorf("check whether instance %s is modded: %w", inst.ID, err)
	}
	if modded {
		return ErrModdedNotConfirmed
	}
	return nil
}

// GameUpdater builds a replacement server tree and commits its swap.
type GameUpdater struct {
	Runtime     runtime.Runtime
	Config      *config.Config
	Snapshotter *Snapshotter
	StageReplay func(context.Context, *store.Instance, string) error
}

func cancelled(ctx context.Context, jh *jobs.Handle) bool {
	return ctx.Err() != nil || jh.CancelRequested(ctx)
}

// updatePhase is one step of the update and the checkpoint that records having finished it.
// A table rather than a run of if-blocks, so the order the phases run in and the order 12 §9.4
// fixes for the checkpoints are the same list.
type updatePhase struct {
	progress   int
	message    string
	checkpoint string
	do         func() error
}

// gameUpdateRun is one run's mutable state. The phases are methods on it rather than closures
// over the runner, so each is readable on its own and the runner is just the order they go in.
type gameUpdateRun struct {
	h        *GameUpdater
	inst     *store.Instance
	cacheDir string
	// buildID is the build the fetch resolved, carried to the clone and to the Finish
	// transaction that records what the instance now runs.
	buildID string
	// archived is the pre-update catalogue row, nil for an instance with no world yet.
	archived func(context.Context, *sql.Tx) error
}

// takeArchive is 05 M4's pre-update backup and 12 §9.4's first checkpoint.
func (r *gameUpdateRun) takeArchive(ctx context.Context, jh *jobs.Handle) error {
	archived, err := r.h.archiveBeforeUpdate(ctx, jh, r.inst)
	if err != nil {
		return err
	}
	r.archived = archived
	return nil
}

// fetchBuild resolves the public branch and puts it in the cache, under the build id the
// downloaded bytes declare rather than the one the lookup returned (Q29).
func (r *gameUpdateRun) fetchBuild(ctx context.Context) error {
	buildID, err := instance.CachePublicBuild(ctx, &instance.BuildCacheInput{
		Runtime:      r.h.Runtime,
		Image:        r.h.Config.Game.SteamCMDImage,
		CacheDir:     r.cacheDir,
		HostCacheDir: instance.CacheDir(r.h.Config.Data.HostRoot),
		HostDataRoot: r.h.Config.Data.HostRoot,
	})
	if err != nil {
		return fmt.Errorf("fetch the current build: %w", err)
	}
	r.buildID = buildID
	return nil
}

// cloneBuild stages the replacement tree beside the live one and asserts A3/A4: the clone
// must already belong to uid 10000, and is never chown'd into place, since a defensive chown
// masks a clone that ran as the wrong user (Q14).
func (r *gameUpdateRun) cloneBuild(ctx context.Context) error {
	if err := instance.StageUpdate(ctx, r.inst.DataDir, r.cacheDir, r.buildID); err != nil {
		return fmt.Errorf("stage build %s: %w", r.buildID, err)
	}
	if err := instance.VerifyClonedOwnership(
		instance.StagedServerDir(r.inst.DataDir), instance.WantCloneUID); err != nil {
		return fmt.Errorf("verify the staged clone: %w", err)
	}
	return nil
}

// phases is the update in the order 12 §9.4 fixes, so that order and the checkpoints that
// record it are one list rather than two things to keep in step.
func (r *gameUpdateRun) phases(ctx context.Context, jh *jobs.Handle) []updatePhase {
	return []updatePhase{
		{5, "backing up the world", RestorePreBackupTaken, func() error { return r.takeArchive(ctx, jh) }},
		{15, "fetching the current public build", UpdateBuildCached, func() error { return r.fetchBuild(ctx) }},
		{35, "cloning the new build", UpdateCloned, func() error { return r.cloneBuild(ctx) }},
		{60, "putting the mods and configs back", UpdateModsReplayed, func() error {
			return r.h.replayOntoStagedServer(ctx, r.inst)
		}},
	}
}

// runGameUpdate builds a complete replacement server beside the live one and swaps it in
// (ADR-138). Nothing under server/ changes until the last step, so every failure and every
// cancellation before it leaves the instance exactly as it was.
//
// worlds/ is never touched. The split in 02 §3 exists so this operation cannot reach it.
func (h *GameUpdater) Run(inst *store.Instance) jobs.Runner {
	stage := instance.UpdateStaging(inst.DataDir)
	run := &gameUpdateRun{h: h, inst: inst, cacheDir: instance.CacheDir(h.Config.Data.Root)}

	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		// Whatever a previous attempt left is discarded rather than continued. Repairing an
		// interrupted run belongs to the crash sweep, which knows how far that run got; here the
		// live server/ is intact by definition, since a job holding the lock is the only thing
		// that swaps.
		if err := os.RemoveAll(stage); err != nil {
			return run.fail(fmt.Errorf("clear a previous update's staging: %w", err))
		}
		swapping := false
		defer func() {
			if !swapping {
				_ = os.RemoveAll(stage)
			}
		}()

		if outcome := run.stage(ctx, jh); outcome != nil {
			return *outcome
		}
		if outcome := run.beginSwap(ctx, jh); outcome != nil {
			return *outcome
		}

		swapping = true
		jh.Progress(ctx, 90, "replacing the server")
		if err := instance.SwapUpdate(inst.DataDir); err != nil {
			return run.fail(err)
		}

		jh.Progress(ctx, 100, "game updated to build "+run.buildID+"; the server is stopped")
		return jobs.Outcome{
			Status:   jobs.StatusSucceeded,
			OnFinish: chainFinish(run.archived, finishGameUpdate(inst.ID, run.buildID)),
			// The staging tree holds the previous server until this point. Removed only once
			// the new build is committed, so a failed Finish leaves the way back on disk.
			AfterFinish: func(context.Context) { _ = os.RemoveAll(stage) },
		}
	}
}

// stage runs every phase up to the swap, returning the outcome to stop on or nil to carry on.
func (r *gameUpdateRun) stage(ctx context.Context, jh *jobs.Handle) *jobs.Outcome {
	for _, phase := range r.phases(ctx, jh) {
		if cancelled(ctx, jh) {
			outcome := r.abandon()
			return &outcome
		}
		jh.Progress(ctx, phase.progress, phase.message)
		if err := phase.do(); err != nil {
			outcome := r.fail(err)
			return &outcome
		}
		if err := jh.Checkpoint(ctx, phase.checkpoint); err != nil {
			outcome := r.fail(err)
			return &outcome
		}
	}
	return nil
}

// beginSwap closes cancellation and proves the server is still down, in that order: both run
// before the checkpoint that closes cancellation, so the policy and the code agree about where
// the point of no return is (12 §8).
func (r *gameUpdateRun) beginSwap(ctx context.Context, jh *jobs.Handle) *jobs.Outcome {
	if cancelled(ctx, jh) {
		outcome := r.abandon()
		return &outcome
	}
	if err := AssertStopped(ctx, r.h.Runtime, r.inst); err != nil {
		outcome := r.fail(err)
		return &outcome
	}
	if err := jh.Checkpoint(ctx, UpdateSwapStarted); err != nil {
		outcome := r.fail(err)
		return &outcome
	}
	return nil
}

// replayOntoStagedServer rebuilds the instance's own layer on the fresh clone: every installed
// package's manifested files, then the user's config tree over the top.
//
// Package files are placed from the manifest by recorded hash, never by re-running 03 §6.4's
// placement heuristics — M2 measured those disagreeing about file separability, which is the
// whole reason the manifest is load-bearing (ADR-009). Configs go last because a shipped
// default must never win over an edit the operator made (ADR-138, B10).
func (h *GameUpdater) replayOntoStagedServer(ctx context.Context, inst *store.Instance) error {
	if h.StageReplay == nil {
		return errors.New("this panel has no mod engine, so installed mods cannot be put back")
	}
	if err := h.StageReplay(ctx, inst, instance.UpdateReplayDir(inst.DataDir)); err != nil {
		return fmt.Errorf("stage the installed mods: %w", err)
	}
	if err := instance.SaveUpdateConfigs(inst.DataDir); err != nil {
		return fmt.Errorf("save the user's configs: %w", err)
	}
	if err := instance.ApplyStagedFiles(inst.DataDir); err != nil {
		return fmt.Errorf("put the mods and configs back: %w", err)
	}
	return nil
}

// archiveBeforeUpdate is 05 M4's pre-update backup and 12 §9.4's first checkpoint. It returns
// the Finish callback that records the archive, so the row lands in the job's own transaction
// (12 §6).
//
// An instance with no worlds/ has nothing to protect — a freshly provisioned server that has
// never run — and updates without an archive rather than being refused one it cannot take.
func (h *GameUpdater) archiveBeforeUpdate(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance,
) (func(context.Context, *sql.Tx) error, error) {
	record, err := h.Snapshotter.Snapshot(ctx, inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating: %w", err)
	}
	if record == nil {
		jh.Log("no pre-update archive was taken: this instance has no world yet")
	}
	return record, nil
}

// abandon is a cancellation before the swap. server/ was never touched and the staging tree is
// about to be deleted, so the instance resolves to `stopped` rather than `error` — the same
// reasoning abandonBackup uses, and the reason `updating` has both exits (12 §2.2).
func (r *gameUpdateRun) abandon() jobs.Outcome {
	return jobs.Outcome{
		Status:   jobs.StatusCancelled,
		OnFinish: chainFinish(r.archived, finishUpdateTo(r.inst.ID, instance.StateStopped)),
	}
}

// fail parks the instance in `error` (B7, ADR-137). No failure here starts a server, and none
// resolves itself: a tree that was being replaced is one a human should look at, even when
// nothing was replaced.
//
// The pre-update archive is recorded whatever happens next. It is the world the operator had,
// and a failed update is when they are most likely to want it.
func (r *gameUpdateRun) fail(err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: FailureCode(err).String(), Error: err.Error(),
		OnFinish: chainFinish(r.archived, finishUpdateTo(r.inst.ID, instance.StateError)),
	}
}

// finishUpdateTo leaves `updating` for one of its two exits (12 §2.2).
func finishUpdateTo(instanceID string, to instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateUpdating, to)
		if err != nil {
			return fmt.Errorf("move instance %s to %s: %w", instanceID, to, err)
		}
		if !ok {
			return fmt.Errorf("instance %s is not in updating state", instanceID)
		}
		return nil
	}
}

// finishGameUpdate records the build the instance now runs alongside its return to `stopped`,
// in one transaction: a row saying it runs a build it does not is what every later update
// check would then read.
func finishGameUpdate(instanceID, buildID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxSetInstanceBuildID(ctx, tx, instanceID, buildID); err != nil {
			return fmt.Errorf("finish game update for instance %s: %w", instanceID, err)
		}
		ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateUpdating, instance.StateStopped)
		if err != nil {
			return fmt.Errorf("finish game update for instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish game update for instance %s: not in updating state", instanceID)
		}
		return nil
	}
}
