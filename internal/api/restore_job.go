package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Checkpoints of a restore job, in order (12 §9.4). They record how far the run got; recovery
// resolves the swap from the directories themselves, which are the only durable truth about a
// rename that was interrupted halfway.
const (
	checkpointPreBackupTaken = "pre_backup_taken"
	checkpointSwapped        = "swapped"
)

// restorePayload is the job's persisted arguments (12 §4.1). It names the archive and nothing
// else: the directories recovery touches are derived from the instance row, so no path on a
// job payload can point the sweep at a tree it does not own.
type restorePayload struct {
	BackupID string `json:"backup_id"`
}

// restoreBackup is POST /instances/{id}/backups/{bid}/restore (04 §3, 12 §3.1).
//
// The one job in the panel that can destroy a world. It requires `stopped`, is never
// cancellable (12 §8), never resumes the server afterwards, and parks the instance in `error`
// on any failure so a human decides what happens next (B7).
func (h *Instances) restoreBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsRestore, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// 12 §3.1's Requires column. A restore never stops a running server for the operator:
	// the world it would replace is the one players are in.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}
	b, ok := h.mustLoadBackup(w, r, id)
	if !ok {
		return
	}

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindRestore, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: restorePayload{BackupID: b.ID},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateRestoring))
			if err != nil {
				return fmt.Errorf("claim restore for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.runRestore(inst, b))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// runRestore is the restore job: prove the archive, snapshot what is there, stage the archive
// beside the world, swap it in.
//
// Every failure parks the instance in `error` (B7): 12 §2.2 gives `restoring` no other exit,
// and for the one job that replaces a world, "it did not work and nothing is running" is the
// state a human should have to acknowledge.
func (h *Instances) runRestore(inst *store.Instance, b *store.Backup) jobs.Runner {
	live := worldsLocalDir(inst)
	staged := live + backup.StagedSuffix

	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		// A previous attempt's staging, if the crash sweep never ran. Removed before anything
		// is written, never merged into.
		_ = os.RemoveAll(staged)

		var snapshot func(context.Context, *sql.Tx) error
		fail := func(code apierr.Code, err error) jobs.Outcome {
			return jobs.Outcome{
				Status: "failed", ErrorCode: code.String(), Error: err.Error(),
				// The pre-restore archive is recorded whether the restore then works or not:
				// it is the world the operator had, and a failure is when they need it most.
				OnFinish: chainFinish(snapshot, finishToError(inst.ID, instance.StateRestoring)),
			}
		}

		// The snapshot is unconditional and first, as 12 §9.4's first checkpoint. Even a
		// restore that turns out to be impossible has already displaced nothing and cost one
		// archive, which is the cheap side of the trade.
		// No prune runs with it: retention is the backup job's step 7, and pruning here could
		// delete the very archive this job is about to read (02 §4.4).
		jh.Progress(ctx, 15, "backing up the world already there")
		taken, err := h.snapshotWorlds(inst, store.TriggerPreRestore)
		if err != nil {
			return fail(apierr.Internal, fmt.Errorf("could not back up the current world: %w", err))
		}
		snapshot = taken
		if err := jh.Checkpoint(ctx, checkpointPreBackupTaken); err != nil {
			return fail(apierr.Internal, err)
		}

		jh.Progress(ctx, 45, "checking the archive")
		if _, err := backup.Verify(b.Path, b.WorldName); err != nil {
			return fail(apierr.BackupUnverifiable, err)
		}

		jh.Progress(ctx, 65, "unpacking the archive")
		if err := stageRestore(b, staged); err != nil {
			_ = os.RemoveAll(staged)
			return fail(apierr.BackupUnverifiable, err)
		}
		if err := jh.Checkpoint(ctx, checkpointStaged); err != nil {
			_ = os.RemoveAll(staged)
			return fail(apierr.Internal, err)
		}

		jh.Progress(ctx, 85, "swapping the world into place")
		if err := backup.Swap(live); err != nil {
			return fail(apierr.Internal, err)
		}
		if err := jh.Checkpoint(ctx, checkpointSwapped); err != nil {
			return fail(apierr.Internal, err)
		}

		jh.Progress(ctx, 100, "world restored")
		return jobs.Outcome{
			Status:   "succeeded",
			OnFinish: chainFinish(snapshot, finishRestore(inst.ID)),
		}
	}
}

// stageRestore unpacks the archive's world directory alongside the live one, on the same
// filesystem so the swap is a rename rather than a second copy of a multi-gigabyte world.
//
// The pair is checked again in the staged tree, because Verify matched it by basename anywhere
// in the archive: an archive whose world sits somewhere the server never looks would otherwise
// swap in an empty world.
func stageRestore(b *store.Backup, staged string) error {
	if err := backup.Extract(b.Path, instance.WorldsLocalDir, staged); err != nil {
		return fmt.Errorf("unpack the archive: %w", err)
	}
	for _, ext := range []string{".db", ".fwl"} {
		// Base, not the raw column: world_name is validated at creation and immutable, and a
		// path join over a database value has no business trusting that twice.
		name := filepath.Base(b.WorldName + ext)
		if fi, err := os.Stat(filepath.Join(staged, name)); err != nil || fi.Size() == 0 {
			return fmt.Errorf("%w: the archive has no %s under %s/",
				backup.ErrWorldMissing, name, instance.WorldsLocalDir)
		}
	}
	return nil
}

// finishRestore is the successful exit: `restoring` back to `stopped`. The server is not
// started, on this path or any other — the operator restarts it once they have looked at the
// world that came back (12 §9.3).
func finishRestore(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := store.TxUpdateInstanceState(
			ctx, tx, instanceID, string(instance.StateRestoring), string(instance.StateStopped))
		if err != nil {
			return fmt.Errorf("finish restore for instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish restore for instance %s: not in restoring state", instanceID)
		}
		return nil
	}
}

// worldsLocalDir is the directory the game keeps saves in, and the one the swap renames.
// Derived from the instance's own data_dir, never from a request or a job payload.
func worldsLocalDir(inst *store.Instance) string {
	return filepath.Join(instance.WorldsDir(inst.DataDir), instance.WorldsLocalDir)
}
