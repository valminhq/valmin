package control

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const (
	restorePreBackupTaken = "pre_backup_taken"
	restoreStaged         = "staged"
	restoreSwapped        = "swapped"
)

type Restorer struct{ Snapshotter *Snapshotter }

// runRestore is the restore job: prove the archive, snapshot what is there, stage the archive
// beside the world, swap it in.
//
// Every failure parks the instance in `error` (B7): 12 §2.2 gives `restoring` no other exit,
// and for the one job that replaces a world, "it did not work and nothing is running" is the
// state a human should have to acknowledge.
func (r *Restorer) Run(inst *store.Instance, b *store.Backup) jobs.Runner {
	live := worldsLocalDir(inst)
	staged := live + backup.StagedSuffix

	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		// A previous attempt's staging, if the crash sweep never ran. Removed before anything
		// is written, never merged into.
		_ = backup.DiscardStaged(live)

		var snapshot func(context.Context, *sql.Tx) error
		fail := func(code errcode.Code, err error) jobs.Outcome {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: code.String(), Error: err.Error(),
				// The pre-restore archive is recorded whether the restore then works or not:
				// it is the world the operator had, and a failure is when they need it most.
				OnFinish: chainFinish(snapshot, finishToError(inst.ID, instance.StateRestoring)),
			}
		}

		// The snapshot is unconditional and first, as 12 §9.4's first checkpoint. Even a
		// restore that turns out to be impossible has already displaced nothing and cost one
		// archive, which is the cheap side of the trade.
		// No prune runs with it: retention is the backup job's step 7, and pruning here could
		// delete the very archive this job is about to read (02 §4.4). The snapshot asks
		// Docker rather than the state column whether the server is down.
		jh.Progress(ctx, 15, "backing up the world already there")
		taken, err := r.Snapshotter.Snapshot(ctx, inst, store.TriggerPreRestore)
		if err != nil {
			return fail(FailureCode(err), fmt.Errorf("could not back up the current world: %w", err))
		}
		snapshot = taken
		if err := jh.Checkpoint(ctx, restorePreBackupTaken); err != nil {
			return fail(errcode.Internal, err)
		}

		jh.Progress(ctx, 45, "checking the archive")
		if _, err := backup.Verify(b.Path, b.WorldName); err != nil {
			return fail(errcode.BackupUnverifiable, err)
		}

		jh.Progress(ctx, 65, "unpacking the archive")
		if err := stageRestore(b, staged); err != nil {
			_ = backup.DiscardStaged(live)
			return fail(errcode.BackupUnverifiable, err)
		}
		// The tree is verified, so it may now say so: a crash from here to the first rename
		// leaves a staging recovery can publish rather than one it has to throw away
		// (ADR-177).
		if err := backup.MarkStaged(live); err != nil {
			_ = backup.DiscardStaged(live)
			return fail(errcode.Internal, err)
		}
		if err := jh.Checkpoint(ctx, restoreStaged); err != nil {
			_ = backup.DiscardStaged(live)
			return fail(errcode.Internal, err)
		}

		// Again, immediately before the rename: staging is the long part, and a server started
		// during it would have the world renamed out from under it (B7). This is the check
		// game_update already makes before replacing a disposable server tree; a world deserves
		// it more.
		if err := AssertStopped(ctx, r.Snapshotter.Runtime, inst); err != nil {
			_ = backup.DiscardStaged(live)
			return fail(FailureCode(err), err)
		}

		jh.Progress(ctx, 85, "swapping the world into place")
		if err := backup.Swap(live); err != nil {
			return fail(errcode.Internal, err)
		}
		if err := jh.Checkpoint(ctx, restoreSwapped); err != nil {
			return fail(errcode.Internal, err)
		}

		jh.Progress(ctx, 100, "world restored")
		return jobs.Outcome{
			Status:   jobs.StatusSucceeded,
			OnFinish: chainFinish(snapshot, finishRestore(inst.ID)),
		}
	}
}

// stageRestore unpacks the archive's world directory alongside the live one, on the same
// filesystem so the swap is a rename rather than a second copy of a multi-gigabyte world.
//
// The world is checked again in the staged tree, because Verify matched it by name anywhere
// in the archive: an archive whose world sits somewhere the server never looks would otherwise
// swap in an empty world. Both of 03 §4's layouts are accepted, since the archive holds
// whichever one the build that wrote it uses (ADR-179).
func stageRestore(b *store.Backup, staged string) error {
	if err := backup.Extract(b.Path, instance.WorldsLocalDir, staged); err != nil {
		return fmt.Errorf("unpack the archive: %w", err)
	}
	// Base, not the raw column: world_name is validated at creation and immutable, and a path
	// join over a database value has no business trusting that twice.
	name := filepath.Base(b.WorldName)
	scan, err := backup.ScanWorlds(staged)
	if err != nil {
		return fmt.Errorf("inspect the staged world: %w", err)
	}
	if !scan.Complete(name) {
		return fmt.Errorf("%w: the archive has no %s under %s/",
			backup.ErrWorldMissing, name, instance.WorldsLocalDir)
	}
	return nil
}

// finishRestore is the successful exit: `restoring` back to `stopped`. The server is not
// started, on this path or any other — the operator restarts it once they have looked at the
// world that came back (12 §9.3).
func finishRestore(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateRestoring, instance.StateStopped)
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

func chainFinish(first, second func(context.Context, *sql.Tx) error) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if first != nil {
			if err := first(ctx, tx); err != nil {
				return err
			}
		}
		return second(ctx, tx)
	}
}

func finishToError(instanceID string, from instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := instance.SetStateTx(ctx, tx, instanceID, from, instance.StateError); err != nil {
			return fmt.Errorf("park instance %s in error: %w", instanceID, err)
		}
		return nil
	}
}
