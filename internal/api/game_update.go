package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Checkpoints of a game_update job, in the order 12 §9.4 fixes. swap_started is the point of
// no return: past it the live server/ is being renamed, and recovery has to finish the job
// rather than discard it.
const (
	checkpointBuildCached  = "build_cached"
	checkpointCloned       = "cloned"
	checkpointModsReplayed = "mods_replayed"
	checkpointSwapStarted  = "swap_started"
)

// errModdedNotConfirmed is 03 §8's rule: nothing updates a modded server without being asked
// twice. A schedule never supplies the second answer, so a tick reports it as a skip
// (ADR-137).
var errModdedNotConfirmed = errors.New("modded instance was not confirmed for a game update")

// moddedConfirmationMessage is what errModdedNotConfirmed reads as to the person who has to
// answer it. 09 §3 puts the consequence next to the choice, in these terms.
const moddedConfirmationMessage = "This instance has mods installed. A game update replaces " +
	"the server files, and the installed mods may not load against the new build. Confirm to continue."

// gameUpdatePayload is the job's persisted arguments (12 §4.1).
type gameUpdatePayload struct {
	ConfirmModded bool `json:"confirm_modded"`
}

// updateGame is POST /instances/{id}/update (04 §3, 12 §3.1). It requires `stopped`, never
// stops a server itself, and never starts one afterwards: the operator starts explicitly, and
// that start is what verifies BepInEx and plugin loading (08 §7 step 6, 03 §5.3).
func (h *Instances) updateGame(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceUpdate, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	var body gameUpdatePayload
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// Checked before the submit, so an unconfirmed modded update creates no job row at all
	// rather than one that fails a second later.
	if err := confirmModded(inst, body.ConfirmModded); err != nil {
		if errors.Is(err, errModdedNotConfirmed) {
			writeFieldError(w, r, "confirm_modded", apierr.FieldRequired, moddedConfirmationMessage)
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	job, err := h.submitGameUpdate(r.Context(), inst, body.ConfirmModded, u.ID, "")
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// confirmModded applies 03 §8: a modded instance needs an explicit yes, a vanilla one needs
// nothing. Doorstop's presence on disk is the test, the same one the entrypoint uses
// (ADR-107) — the installed-mods table would answer a different question, since a hand-placed
// framework is still a framework the update will replace.
func confirmModded(inst *store.Instance, confirmed bool) error {
	if confirmed {
		return nil
	}
	modded, err := instance.HasDoorstop(inst.DataDir)
	if err != nil {
		return fmt.Errorf("check whether instance %s is modded: %w", inst.ID, err)
	}
	if modded {
		return errModdedNotConfirmed
	}
	return nil
}

// gameUpdateCancelPolicy is 12 §8's point of no return for this kind: cancellable right up to
// the rename, and not after it. Everything before the swap leaves server/ untouched, so
// cancelling costs a download and nothing else.
func gameUpdateCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	if checkpoint == checkpointSwapStarted {
		return false, "the swap"
	}
	return true, ""
}

// submitGameUpdate is the one path a game_update job is created through, whether an operator
// asked for it or a schedule's tick did. requestedBy is empty for the scheduler, which writes
// NULL (12 §11).
func (h *Instances) submitGameUpdate(
	ctx context.Context, inst *store.Instance, confirmed bool, requestedBy, scheduleID string,
) (*store.Job, error) {
	if err := confirmModded(inst, confirmed); err != nil {
		return nil, err
	}
	id := inst.ID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindGameUpdate, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name,
		RequestedBy: requestedBy, ScheduleID: scheduleID,
		Payload: gameUpdatePayload{ConfirmModded: confirmed},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateUpdating))
			if err != nil {
				return fmt.Errorf("claim game update for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.runGameUpdate(inst))
	if err != nil {
		// Wrapped, not replaced: writeJobSubmitError and the scheduler's skip both reach
		// through this with errors.As to find *store.JobConflict.
		return nil, fmt.Errorf("submit game update for instance %s: %w", id, err)
	}
	return job, nil
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
	h        *Instances
	inst     *store.Instance
	cacheDir string
	// buildID is the build the fetch resolved, carried to the clone and to the Finish
	// transaction that records what the instance now runs.
	buildID string
	// archived is the pre-update catalogue row, nil for an instance with no world yet.
	archived func(context.Context, *sql.Tx) error
}

// takeArchive is 05 M4's pre-update backup and 12 §9.4's first checkpoint.
func (r *gameUpdateRun) takeArchive(jh *jobs.Handle) error {
	archived, err := r.h.archiveBeforeUpdate(jh, r.inst)
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
		Runtime: r.h.Runtime, Image: r.h.Cfg.Game.SteamCMDImage,
		CacheDir: r.cacheDir, HostCacheDir: instance.CacheDir(r.h.Cfg.Data.HostRoot),
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
		{5, "backing up the world", checkpointPreBackupTaken, func() error { return r.takeArchive(jh) }},
		{15, "fetching the current public build", checkpointBuildCached, func() error { return r.fetchBuild(ctx) }},
		{35, "cloning the new build", checkpointCloned, func() error { return r.cloneBuild(ctx) }},
		{60, "putting the mods and configs back", checkpointModsReplayed, func() error {
			return r.h.replayOntoStagedServer(ctx, r.inst)
		}},
	}
}

// runGameUpdate builds a complete replacement server beside the live one and swaps it in
// (ADR-138). Nothing under server/ changes until the last step, so every failure and every
// cancellation before it leaves the instance exactly as it was.
//
// worlds/ is never touched. The split in 02 §3 exists so this operation cannot reach it.
func (h *Instances) runGameUpdate(inst *store.Instance) jobs.Runner {
	stage := instance.UpdateStaging(inst.DataDir)
	run := &gameUpdateRun{h: h, inst: inst, cacheDir: instance.CacheDir(h.Cfg.Data.Root)}

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
			Status:   "succeeded",
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
	if err := r.h.assertStopped(ctx, r.inst); err != nil {
		outcome := r.fail(err)
		return &outcome
	}
	if err := jh.Checkpoint(ctx, checkpointSwapStarted); err != nil {
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
func (h *Instances) replayOntoStagedServer(ctx context.Context, inst *store.Instance) error {
	if h.Mods == nil {
		return errors.New("this panel has no mod engine, so installed mods cannot be put back")
	}
	if err := h.Mods.StageReplay(ctx, inst, instance.UpdateReplayDir(inst.DataDir)); err != nil {
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
func (h *Instances) archiveBeforeUpdate(
	jh *jobs.Handle, inst *store.Instance,
) (func(context.Context, *sql.Tx) error, error) {
	record, err := h.snapshotWorlds(inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating: %w", err)
	}
	if record == nil {
		jh.Log("no pre-update archive was taken: this instance has no world yet")
	}
	return record, nil
}

// assertStopped re-reads Docker immediately before the swap. The state column said `stopped`
// when the lock was taken, and the lock keeps the panel out; it does not keep out an operator
// with a docker CLI, and renaming a tree out from under a running server is unrecoverable.
func (h *Instances) assertStopped(ctx context.Context, inst *store.Instance) error {
	if inst.ContainerID == nil {
		return nil
	}
	c, err := h.Runtime.Inspect(ctx, *inst.ContainerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check that instance %s is stopped: %w", inst.ID, err)
	}
	if c.Running {
		return errors.New("the server started while the update was staging, so nothing was replaced")
	}
	return nil
}

// cancelled reports whether the job should stop: either the lease is gone or a person asked.
func cancelled(ctx context.Context, jh *jobs.Handle) bool {
	return ctx.Err() != nil || jh.CancelRequested(ctx)
}

// abandon is a cancellation before the swap. server/ was never touched and the staging tree is
// about to be deleted, so the instance resolves to `stopped` rather than `error` — the same
// reasoning abandonBackup uses, and the reason `updating` has both exits (12 §2.2).
func (r *gameUpdateRun) abandon() jobs.Outcome {
	return jobs.Outcome{
		Status:   "cancelled",
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
		Status: "failed", ErrorCode: apierr.Internal.String(), Error: err.Error(),
		OnFinish: chainFinish(r.archived, finishUpdateTo(r.inst.ID, instance.StateError)),
	}
}

// finishUpdateTo leaves `updating` for one of its two exits (12 §2.2).
func finishUpdateTo(instanceID string, to instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := store.TxUpdateInstanceState(
			ctx, tx, instanceID, string(instance.StateUpdating), string(to))
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
		ok, err := store.TxUpdateInstanceState(
			ctx, tx, instanceID, string(instance.StateUpdating), string(instance.StateStopped))
		if err != nil {
			return fmt.Errorf("finish game update for instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish game update for instance %s: not in updating state", instanceID)
		}
		return nil
	}
}
