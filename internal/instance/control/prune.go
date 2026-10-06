package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Pruner runs global archive retention without holding a transaction across file work.
type Pruner struct {
	DB     *store.DB
	Engine *jobs.Engine
}

// Submit enqueues retention under the global prune lock.
func (h *Pruner) Submit(ctx context.Context, scheduleID string) (*store.Job, error) {
	j, err := h.Engine.Submit(ctx, PruneSpec(scheduleID), h.Run)
	if err != nil {
		return nil, fmt.Errorf("submit prune: %w", err)
	}
	return j, nil
}

// SetupArtifactLock keeps archive pruning away from setup jobs using their source archives.
const SetupArtifactLock = "global:setup-artifacts"

// PruneSpec defines the global retention job and its shared setup lock.
func PruneSpec(scheduleID string) *jobs.Spec {
	return &jobs.Spec{
		Kind: jobs.KindPrune, LockKey: jobs.GlobalLockKey(jobs.KindPrune),
		LockKeys: []string{SetupArtifactLock}, Payload: struct{}{}, ScheduleID: scheduleID,
	}
}

// Run sweeps every instance through the same retention helper a backup job's step 7 uses.
// Idempotent: a second run over a catalogue already inside its policy deletes nothing, which is
// what makes it one of the kinds 12 §9.4 allows to simply re-run.
func (h *Pruner) Run(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
			Error: fmt.Sprintf("read the instances: %v", err),
		}
	}

	// Collected across instances and deleted in one Finish transaction. File removal follows
	// that commit, so a crash can leave an orphan file but never a broken catalogue row.
	type pruned struct {
		instanceID string
		archives   []backup.Entry
	}
	var all []pruned
	total := 0

	for i := range instances {
		inst := &instances[i]
		if ctx.Err() != nil {
			break
		}
		doomed, err := h.Select(ctx, inst, nil)
		if err != nil {
			// One unreadable catalogue or one file that will not unlink must not stop the
			// sweep: the other instances are still over their retention.
			jh.Log(fmt.Sprintf("%s: %v", inst.Name, err))
			continue
		}
		if len(doomed) == 0 {
			continue
		}
		all = append(all, pruned{instanceID: inst.ID, archives: doomed})
		total += len(doomed)
		jh.Progress(ctx, 100*(i+1)/len(instances), fmt.Sprintf("pruned %d archives", total))
	}

	return jobs.Outcome{
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			for _, p := range all {
				for _, a := range p.archives {
					if err := pruneBackupRow(ctx, tx, p.instanceID, a.ID); err != nil {
						return err
					}
				}
			}
			return nil
		},
		AfterFinish: func(ctx context.Context) {
			for _, p := range all {
				h.Cleanup(p.instanceID, p.archives)(ctx)
			}
		},
	}
}

const pruneScanLimit = 500

// Select applies retention to the complete catalogue, including a fresh archive when supplied.
func (h *Pruner) Select(
	ctx context.Context, inst *store.Instance, fresh *store.Backup,
) ([]backup.Entry, error) {
	entries := make([]backup.Entry, 0, pruneScanLimit+1)
	if fresh != nil {
		entries = append(entries, pruneEntry(fresh))
	}
	var beforeCreatedAt, beforeID string
	for {
		rows, err := h.DB.ListBackups(ctx, inst.ID, beforeCreatedAt, beforeID, pruneScanLimit)
		if err != nil {
			return nil, fmt.Errorf("read the catalogue: %w", err)
		}
		for i := range rows {
			pinned, err := h.DB.BackupPinned(ctx, inst.ID, rows[i].ID)
			if err != nil {
				return nil, fmt.Errorf("check backup %s links: %w", rows[i].ID, err)
			}
			entry := pruneEntry(&rows[i])
			protected, err := h.DB.RemoteBackupProtected(ctx, inst.ID, rows[i].ID)
			if err != nil {
				return nil, fmt.Errorf("check remote protection: %w", err)
			}
			entry.Pinned = pinned || protected
			entries = append(entries, entry)
		}
		if len(rows) < pruneScanLimit {
			break
		}
		last := rows[len(rows)-1]
		beforeCreatedAt, beforeID = store.FormatTime(last.CreatedAt), last.ID
	}
	return backup.Prune(entries, backup.Policy{
		KeepCold: inst.BackupKeepCold, KeepHot: inst.BackupKeepHot,
	}), nil
}

func pruneEntry(b *store.Backup) backup.Entry {
	return backup.Entry{
		ID: b.ID, Path: b.Path, Consistent: b.Consistent,
		Snapshot: b.Trigger == store.TriggerPreUpdate ||
			b.Trigger == store.TriggerPreRestore ||
			b.Trigger == store.TriggerPreImport,
	}
}

// Cleanup removes pruned files only after their catalogue rows are gone.
func (h *Pruner) Cleanup(instanceID string, entries []backup.Entry) func(context.Context) {
	if len(entries) == 0 {
		return nil
	}
	return func(ctx context.Context) {
		for _, entry := range entries {
			remaining, err := h.DB.BackupByID(ctx, instanceID, entry.ID)
			if err != nil || remaining != nil {
				continue
			}
			if err := backup.Remove(entry); err != nil {
				slog.WarnContext(ctx, "remove pruned archive",
					slog.String("instance_id", instanceID),
					slog.String("backup_id", entry.ID),
					slog.Any("error", err))
			}
		}
	}
}

func pruneBackupRow(ctx context.Context, tx *sql.Tx, instanceID, backupID string) error {
	if err := store.TxDeleteBackup(
		ctx,
		tx,
		instanceID,
		backupID,
	); err != nil &&
		!errors.Is(err, store.ErrBackupProtected) {
		return fmt.Errorf("prune archive %s: %w", backupID, err)
	}
	return nil
}
