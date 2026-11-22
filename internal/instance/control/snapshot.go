package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Worlds runs the jobs that replace, import or delete a world, each after archiving the worlds
// already there.
type Worlds struct {
	DataRoot string
	Runtime  runtime.Runtime
}

// Snapshot archives an instance's worlds/ under trigger and returns the OnFinish that
// records it, so the catalogue row lands in the job's own Finish transaction from data already
// in memory (12 §6) — and never before the archive file itself exists. A nil callback means
// there was nothing to archive. snapshotRow says what it guarantees.
func (h *Worlds) Snapshot(
	ctx context.Context, inst *store.Instance, trigger string,
) (func(context.Context, *sql.Tx) error, error) {
	row, err := h.snapshotRow(ctx, inst, trigger)
	if err != nil || row == nil {
		return nil, err
	}
	return recordSnapshot(row), nil
}

// snapshotRow archives an instance's worlds/ under trigger and returns the catalogue row to
// record, nil when there was nothing to archive.
//
// It returns without error only once Docker has shown the server down, after the copy when
// there was one, so a caller that writes worlds/ straight after it writes under a stopped
// server. A running server fails the supplied stop check.
//
// It does not verify what it captured, unlike the backup job: the worlds it protects are the
// ones about to be replaced, and a world worth restoring away from is often one that would
// fail verification. The archive is still recorded consistent, which is 02 §4.4's claim about
// a stopped server rather than about the bytes.
func (h *Worlds) snapshotRow(ctx context.Context, inst *store.Instance, trigger string) (*store.Backup, error) {
	empty, err := holdsNoFiles(instance.WorldsDir(inst.DataDir))
	if err != nil {
		return nil, fmt.Errorf("inspect the worlds of %s: %w", inst.ID, err)
	}
	if empty {
		// Nothing to lose yet — a first import into a fresh instance, which is still a write.
		return nil, AssertStopped(ctx, h.Runtime, inst)
	}

	backupID := store.NewID()
	dest := filepath.Join(instance.BackupsDir(h.DataRoot), inst.ID,
		backup.Name(inst.Name, time.Now().UTC().Format("20060102T150405Z"), backupID))
	res, err := h.archiveStoppedWorlds(ctx, inst, dest)
	if err != nil {
		return nil, err
	}
	return &store.Backup{
		ID: backupID, InstanceID: inst.ID, Path: res.Path,
		SizeBytes: res.SizeBytes, SHA256: res.SHA256, WorldName: inst.WorldName,
		Trigger: trigger,
		// ArchiveStoppedWorlds saw the server down on both sides of the copy.
		Consistent: true,
	}, nil
}

// recordSnapshot is the finish step that catalogues a snapshot row.
func recordSnapshot(row *store.Backup) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxCreateBackup(ctx, tx, row); err != nil {
			return fmt.Errorf("record the %s backup: %w", row.Trigger, err)
		}
		return nil
	}
}

// holdsNoFiles reports whether dir is missing or has no file anywhere below it, which an
// archive would capture as nothing.
func holdsNoFiles(dir string) (bool, error) {
	empty := true
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			empty = false
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("walk %s: %w", dir, err)
	}
	return empty, nil
}

// archiveStoppedWorlds archives inst's worlds/ to dest, checking before and after the copy
// whether the server is running. Only an archive the server was down for the whole of may be
// catalogued as consistent; one it started during is removed and the stop check's error is returned.
func (h *Worlds) archiveStoppedWorlds(
	ctx context.Context, inst *store.Instance, dest string,
) (backup.Result, error) {
	if err := AssertStopped(ctx, h.Runtime, inst); err != nil {
		return backup.Result{}, err
	}
	// worldsDir is data_dir + "worlds"; data_dir is panel-generated and no user string
	// reaches the column (checked again by the delete job's own root guard).
	worldsDir := filepath.Clean(instance.WorldsDir(inst.DataDir))
	res, err := backup.Archive(worldsDir, dest)
	if err != nil {
		return backup.Result{}, fmt.Errorf("archive %s: %w", worldsDir, err)
	}
	if err := AssertStopped(ctx, h.Runtime, inst); err != nil {
		_ = os.Remove(res.Path)
		return backup.Result{}, err
	}
	return res, nil
}
