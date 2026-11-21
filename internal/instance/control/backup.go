package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Backupper owns world archive jobs and restart snapshots.
type Backupper struct {
	DB       *store.DB
	Engine   *jobs.Engine
	Runtime  runtime.Runtime
	Stopper  *Stopper
	Starter  *Starter
	DataRoot string
	// FreeSpace reads the data root's free bytes. With FreeSpaceFloor, it makes a stop skip its
	// archive while the disk is below the floor. Nil checks nothing.
	FreeSpace      func() (uint64, error)
	FreeSpaceFloor uint64
}

// BackupRun is one backup job: the server, the archive it writes and how it was asked for.
type BackupRun struct {
	Instance    *store.Instance
	ContainerID string
	Mode        BackupMode
	BackupID    string
	Dest        string
	Trigger     string
	// WasRunning is whether the server ran when the backup was submitted.
	WasRunning bool
}

// Run is the backup job's Runner (02 §4.4).
func (b *Backupper) Run(run *BackupRun) jobs.Runner {
	inst, containerID, mode := run.Instance, run.ContainerID, run.Mode
	backupID, dest, trigger, wasRunning := run.BackupID, run.Dest, run.Trigger, run.WasRunning
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		quiescing := mode == BackupQuiesced && wasRunning
		// resume is what the server is owed once this job lets go of the lock. It is attached
		// to every outcome below, not only the successful one: the operator asked for a
		// backup, not a shutdown, and a failed 3 a.m. backup that leaves the server down until
		// morning has done more harm than the missing archive.
		resume := b.resumeAfterBackup(inst, containerID, quiescing)
		if quiescing {
			out, parked, ok := b.quiesce(ctx, jh, inst.ID, containerID)
			if !ok {
				// Parked in `error`: on-disk state is not the question, but 12 §2.4 permits no
				// start from `error`, and a stop that failed or was force-killed is exactly the
				// case a human should look at.
				if !parked {
					out.AfterFinish = resume
				}
				return out
			}
		}

		consistent, out, ok := b.consistencyClaim(ctx, jh, inst, mode, wasRunning, resume)
		if !ok {
			return out
		}
		if jh.CancelRequested(ctx) {
			return b.abandonBackup(ctx, inst.ID, quiescing, jobs.Outcome{
				Status: jobs.StatusCancelled, AfterFinish: resume,
			})
		}

		jh.Progress(ctx, 55, "archiving the world")
		row, err := b.archiveAndVerify(ctx, jh, inst, backupID, dest, trigger, consistent)
		if err != nil {
			return b.abandonBackup(ctx, inst.ID, quiescing, jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: archiveFailureCode(err),
				Error: err.Error(), AfterFinish: resume,
			})
		}

		jh.Progress(ctx, 90, "pruning old archives")
		pruned, err := (&Pruner{DB: b.DB}).Select(ctx, inst, row)
		if err != nil {
			// The archive is written and verified; failing the job now would discard a good
			// backup over housekeeping. Say so and succeed.
			jh.Log("could not prune old archives: " + err.Error())
		}
		logPruned(jh, pruned)

		jh.Progress(ctx, 100, backupMessage(consistent, len(pruned)))
		return jobs.Outcome{
			Status:   jobs.StatusSucceeded,
			OnFinish: finishBackup(inst.ID, quiescing, row, pruned),
			// Files are removed only after their catalogue rows commit. The chained start of
			// 12 §2.3 follows that cleanup after the lock is released (12 §9.3).
			AfterFinish: chainAfterFinish((&Pruner{DB: b.DB}).Cleanup(inst.ID, pruned), resume),
		}
	}
}

// archiveFailureCode separates an archive that could not be proved complete from one that could
// not be written at all: the first is BackupUnverifiable and publishes nothing (B8).
func archiveFailureCode(err error) string {
	if isUnverifiable(err) {
		return errcode.BackupUnverifiable.String()
	}
	return errcode.Internal.String()
}

// consistencyClaim decides whether this archive may be recorded as consistent, establishing
// against Docker that nothing is writing the world it is about to read.
//
// wasRunning came from instances.state, which is the panel's record of its own intentions: it
// says `stopped` for a container an operator started with a docker CLI, and the instance lock
// excludes panel jobs rather than that operator. Consistency is a claim about the bytes, so it
// is checked against the engine (B2, B12).
//
// A running server is fatal to the quiesced mode, which promised an archive it can no longer
// take: the claim transaction never entered `stopping`, so nothing owes this server a restart
// and stopping it here would be a shutdown the operator did not ask for. A hot copy is a
// defined operation over a live world and only loses its consistency claim.
func (b *Backupper) consistencyClaim(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance,
	mode BackupMode, wasRunning bool, resume func(context.Context),
) (consistent bool, out jobs.Outcome, ok bool) {
	// A hot copy of a running server is the one archive taken over a live world, so it is the
	// one that cannot claim consistency (B12). Everything else claims it, and therefore has to
	// prove it.
	if mode == BackupHot && wasRunning {
		return false, jobs.Outcome{}, true
	}
	running, err := runningInDocker(ctx, b.Runtime, inst)
	if err != nil {
		return false, jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: err.Error(), AfterFinish: resume,
		}, false
	}
	if !running {
		return true, jobs.Outcome{}, true
	}
	if mode == BackupHot {
		jh.Log("the server is running although this instance is recorded as stopped; " +
			"the archive is a live copy and is not marked consistent")
		return false, jobs.Outcome{}, true
	}
	return false, jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: errcode.BackupUnverifiable.String(),
		Error: "the server is running although this instance is recorded as stopped, " +
			"so no archive was taken",
		AfterFinish: resume,
	}, false
}

// quiesce is 02 §4.4 steps 2 and 3: stop the server and require the save-complete line.
// Where `stop` records clean=false and carries on, backup refuses to archive — a catalogue
// holding a file marked consistent that is not is worse than no backup (12 §3.4).
// The second return reports whether the instance was parked in `error`, which decides whether
// the server may be started again afterwards.
func (b *Backupper) quiesce(
	ctx context.Context, jh *jobs.Handle, instanceID, containerID string,
) (out jobs.Outcome, parked, ok bool) {
	jh.Progress(ctx, 20, "stopping the server")
	clean, killed, err := b.Stopper.stopContainer(ctx, containerID)
	if out := stopFailure(jh, instanceID, killed, err); out != nil {
		return *out, true, false
	}

	if _, err := instance.SetState(
		ctx, b.DB, instanceID, instance.StateStopping, instance.StateStopped); err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: fmt.Sprintf("move instance %s to stopped: %v", instanceID, err),
		}, false, false
	}
	if !clean {
		no := false
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.BackupUnverifiable.String(), Clean: &no,
			Error: "the server stopped without confirming it had written the world, " +
				"so no archive was taken",
		}, false, false
	}

	if _, err := instance.SetState(
		ctx, b.DB, instanceID, instance.StateStopped, instance.StateBackingUp); err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: fmt.Sprintf("move instance %s to backing_up: %v", instanceID, err),
		}, false, false
	}
	return jobs.Outcome{}, false, true
}

// archiveAndVerify writes the archive and proves it is one before any row names it
// (02 §4.4 steps 4 and 5). A verification failure removes the file: an archive nothing
// vouches for must not be left where a later operator reads it as a backup. A verified archive
// also records its world's size and what the world check found, which never refuses it.
func (b *Backupper) archiveAndVerify(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance,
	backupID, dest, trigger string, consistent bool,
) (*store.Backup, error) {
	archive := backup.Archive
	if !consistent {
		archive = backup.ArchiveLive
	}
	res, err := archive(instance.WorldsDir(inst.DataDir), dest)
	if err != nil {
		return nil, fmt.Errorf("archive the world: %w", err)
	}
	row := &store.Backup{
		ID: backupID, InstanceID: inst.ID, Path: res.Path,
		SizeBytes: res.SizeBytes, SHA256: res.SHA256, WorldName: inst.WorldName,
		Trigger: trigger, Consistent: consistent,
	}
	// A snapshot is the copy taken of the world about to be changed. A world worth going back to
	// is often one that would fail verification, so a snapshot is kept as it was copied.
	if store.SnapshotTrigger(trigger) {
		return row, nil
	}
	found, err := backup.Verify(dest, inst.WorldName)
	if err != nil {
		// dest is panel-built from data.root, the instance id and a timestamp; no request
		// value reaches it.
		_ = os.Remove(dest)
		return nil, fmt.Errorf("verify the archive: %w", err)
	}
	attention := found.Attention
	if shrunk, ok, err := b.shrinkage(ctx, inst, found.WorldBytes); err != nil {
		// The check flags and never refuses, so a failed read costs the comparison, not the archive.
		jh.Log("could not compare the world with earlier backups: " + err.Error())
	} else if ok {
		attention = append(attention, shrunk)
	}
	if found.Unchecked != "" {
		jh.Log(found.Unchecked)
	}
	for _, a := range attention {
		jh.Log("attention required, check the server log around this save: " + a.Message)
	}
	row.WorldBytes, row.WorldAttention = &found.WorldBytes, attention
	return row, nil
}

// shrinkage compares a world of worldBytes with the last archive of it that passed the world
// check, reporting the finding when it shrank to under half.
func (b *Backupper) shrinkage(
	ctx context.Context, inst *store.Instance, worldBytes int64,
) (backup.Attention, bool, error) {
	baseline, at, ok, err := b.DB.WorldBaseline(ctx, inst.ID, inst.WorldName)
	if err != nil {
		return backup.Attention{}, false, fmt.Errorf("read the world baseline: %w", err)
	}
	if !ok || !backup.Shrunk(worldBytes, baseline) {
		return backup.Attention{}, false, nil
	}
	return backup.ShrunkAttention(worldBytes, baseline, at), true, nil
}

// ArchivePath is where an archive of inst identified by backupID lands. Built from data.root,
// the instance id and that id, so no request value reaches it (D13).
func ArchivePath(dataRoot string, inst *store.Instance, backupID string) string {
	return filepath.Join(instance.BackupsDir(dataRoot), inst.ID,
		backup.Name(inst.Name, time.Now().UTC().Format("20060102T150405Z"), backupID))
}

// isUnverifiable reports whether err is the archive refusing to vouch for itself rather than
// the panel failing, so the job carries the registry code an operator can act on.
func isUnverifiable(err error) bool {
	return errors.Is(err, backup.ErrArchiveUnreadable) ||
		errors.Is(err, backup.ErrWorldMissing) ||
		errors.Is(err, backup.ErrWorldImplausible)
}

func chainAfterFinish(callbacks ...func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		for _, callback := range callbacks {
			if callback != nil {
				callback(ctx)
			}
		}
	}
}

// finishBackup writes the new catalogue row and removes the pruned ones, alongside the state
// flip, in the job's own Finish transaction (12 §6).
func finishBackup(
	instanceID string, quiescing bool, row *store.Backup, pruned []backup.Entry,
) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxCreateBackup(ctx, tx, row); err != nil {
			return fmt.Errorf("record backup for instance %s: %w", instanceID, err)
		}
		for _, a := range pruned {
			if err := store.TxDeleteBackup(ctx, tx, instanceID, a.ID); err != nil {
				if errors.Is(err, store.ErrBackupProtected) {
					continue
				}
				return fmt.Errorf("prune archive %s: %w", a.ID, err)
			}
		}
		if !quiescing {
			return nil
		}
		ok, err := instance.SetStateTx(ctx, tx, instanceID, instance.StateBackingUp, instance.StateStopped)
		if err != nil {
			return fmt.Errorf("finish backup for instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish backup for instance %s: not in backing_up state", instanceID)
		}
		return nil
	}
}

// abandonBackup returns out with the transition a quiesced job still owes: the instance is in
// `backing_up` or `stopping` and must not be left there. The world was never touched, so it
// resolves to `stopped` rather than `error`.
func (b *Backupper) abandonBackup(
	ctx context.Context, instanceID string, quiescing bool, out jobs.Outcome,
) jobs.Outcome {
	if !quiescing || out.OnFinish != nil {
		return out
	}
	if _, err := instance.SetState(
		ctx, b.DB, instanceID, instance.StateBackingUp, instance.StateStopped); err != nil {
		out.Error += fmt.Sprintf(" (and instance %s could not be returned to stopped: %v)",
			instanceID, err)
	}
	return out
}

// resumeAfterBackup starts the server the job stopped, once the Finish transaction has
// committed and the lock is released — a job cannot submit another on its own lock key while
// holding it (12 §2.3, §9.3).
func (b *Backupper) resumeAfterBackup(
	inst *store.Instance, containerID string, quiescing bool,
) func(context.Context) {
	if !quiescing {
		return nil
	}
	return func(ctx context.Context) {
		if _, err := b.Starter.Submit(ctx, &StartSubmission{Instance: inst, ContainerID: containerID}); err != nil {
			slog.WarnContext(ctx, "could not restart the server after its backup",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
		}
	}
}

// backupMessage is a finished backup job's progress line, with how many archives retention removed.
func backupMessage(consistent bool, pruned int) string {
	msg := "Backup complete"
	if !consistent {
		msg = "Hot copy complete (taken while the server was running, so it is best-effort)"
	}
	switch pruned {
	case 0:
		return msg
	case 1:
		return msg + "; retention removed 1 older backup"
	default:
		return fmt.Sprintf("%s; retention removed %d older backups", msg, pruned)
	}
}

// logPruned names in the job's log each archive retention is about to remove.
func logPruned(jh *jobs.Handle, pruned []backup.Entry) {
	for _, e := range pruned {
		jh.Log("retention removes " + filepath.Base(e.Path))
	}
}

// noRoom says why a stop must not write an archive, or returns "" when it may. A stop is never
// refused for want of disk, so unlike a backup job it gives up the archive instead. Free space
// that cannot be read counts as no room.
func (b *Backupper) noRoom() string {
	if b.FreeSpace == nil {
		return ""
	}
	free, err := b.FreeSpace()
	if err != nil {
		return fmt.Sprintf("the data root's free space could not be read (%v)", err)
	}
	if free < b.FreeSpaceFloor {
		return fmt.Sprintf("the data root has %d MiB free, below the floor of %d MiB",
			free>>20, b.FreeSpaceFloor>>20)
	}
	return ""
}

// planArchive names the archive a stop or restart of inst may write, or returns an empty plan
// when none is owed: the stop did not ask for one, or the instance has not opted in. It is
// called when the job is queued and the plan stored in the job's payload, so that if the panel
// dies while the archive is written, the next start can remove exactly that file.
func (b *Backupper) planArchive(inst *store.Instance, asked bool) ArchivePlan {
	if b == nil || !asked || !inst.BackupOnRestart {
		return ArchivePlan{}
	}
	id := store.NewID()
	return ArchivePlan{ArchiveID: id, ArchivePath: ArchivePath(b.DataRoot, inst, id)}
}

// archiveAfterStop takes the cold archive a stop can have almost for free: it runs once the
// container is down and the world already flushed, to the file the plan names. what names the
// stop in the job log, such as "restart" or "stop". An empty plan takes none, since the archive
// adds its own duration to every stop that has one.
//
// clean is the stop's save-complete signal, and gates the whole thing: an archive taken after
// an unconfirmed save would be recorded consistent when it is not. A stop tolerates a missing
// save line where a backup does not, so here it costs the archive rather than the job.
//
// It returns the Finish callback that records the archive, or nil. A failure never fails the
// stop: no row is written and the job's log says why.
func (b *Backupper) archiveAfterStop(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, plan ArchivePlan, clean bool, what string,
) (finish func(context.Context, *sql.Tx) error, cleanup func(context.Context)) {
	if plan.ArchivePath == "" {
		return nil, nil
	}
	if !clean {
		jh.Log("no archive was taken on this " + what + ": the server stopped without confirming " +
			"it had written the world")
		return nil, nil
	}
	if reason := b.noRoom(); reason != "" {
		jh.Log("no archive was taken on this " + what + ": " + reason)
		return nil, nil
	}

	jh.Progress(ctx, 40, "archiving the world")
	row, err := b.archiveAndVerify(
		ctx, jh, inst, plan.ArchiveID, plan.ArchivePath, store.TriggerManual, true,
	)
	if err != nil {
		jh.Log("no archive was taken on this " + what + ": " + err.Error())
		return nil, nil
	}
	pruner := &Pruner{DB: b.DB}
	pruned, err := pruner.Select(ctx, inst, row)
	if err != nil {
		jh.Log("could not prune old archives: " + err.Error())
	}
	logPruned(jh, pruned)
	record := finishBackup(inst.ID, false, row, pruned)
	var unrecorded atomic.Bool
	finish = func(ctx context.Context, tx *sql.Tx) error { return recordArchive(ctx, tx, record, &unrecorded) }
	cleanup = func(ctx context.Context) {
		if !unrecorded.Load() {
			if prunedFiles := pruner.Cleanup(inst.ID, pruned); prunedFiles != nil {
				prunedFiles(ctx)
			}
			return
		}
		if err := os.Remove(plan.ArchivePath); err != nil && !os.IsNotExist(err) {
			slog.WarnContext(ctx, "could not remove an archive the catalogue refused",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
		}
	}
	return finish, cleanup
}

// recordArchive runs record, which catalogues a stop's archive, in a savepoint of the job's finish
// transaction. An archive the catalogue cannot take must not keep the stop from finishing, which
// would leave the server locked in stopping: the savepoint undoes whatever record wrote, unrecorded
// is set, and the caller removes the file once the transaction has committed.
func recordArchive(
	ctx context.Context, tx *sql.Tx, record func(context.Context, *sql.Tx) error, unrecorded *atomic.Bool,
) error {
	const savepoint = "stop_archive"
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
		return fmt.Errorf("begin the archive savepoint: %w", err)
	}
	if err := record(ctx, tx); err != nil {
		slog.WarnContext(ctx, "the archive a stop wrote could not be recorded; the stop finishes without it",
			slog.Any("error", err))
		unrecorded.Store(true)
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO "+savepoint); err != nil {
			return fmt.Errorf("undo the archive record: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE "+savepoint); err != nil {
		return fmt.Errorf("end the archive savepoint: %w", err)
	}
	return nil
}
