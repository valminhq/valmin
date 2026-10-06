package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Deleter removes an instance's container and disposable files before deleting its row.
type Deleter struct {
	Runtime   runtime.Runtime
	DataRoot  string
	RemoveAll func(string) error
}

// Run is idempotent so recovery can submit another delete after an interruption.
func (d Deleter) Run(instanceID, containerID, dataDir string, keepWorlds bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "removing container")
		if containerID != "" {
			if err := d.Runtime.Remove(ctx, containerID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return jobs.Outcome{
					Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(),
					Error: fmt.Sprintf("remove container: %v", err),
				}
			}
		}
		jh.Progress(ctx, 60, "removing files")
		if err := d.deleteInstanceFiles(instanceID, dataDir, keepWorlds); err != nil {
			return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
		}
		jh.Progress(ctx, 100, "deleted")
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxDeleteInstance(ctx, tx, instanceID, string(instance.StateDeleting))
			},
		}
	}
}

func (d Deleter) deleteInstanceFiles(instanceID, dataDir string, keepWorlds bool) error {
	root := filepath.Join(d.DataRoot, "instances")
	dir := filepath.Clean(dataDir)
	if !deleteWithinRoot(root, dir) {
		return fmt.Errorf("refusing path outside %s", root)
	}
	backupRoot := instance.BackupsDir(d.DataRoot)
	backupDir := filepath.Join(backupRoot, instanceID)
	if !deleteWithinRoot(backupRoot, backupDir) {
		return fmt.Errorf("refusing backup path outside %s", backupRoot)
	}
	if !keepWorlds {
		for _, path := range []string{dir, backupDir} {
			if err := d.removeInstanceFiles(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
		return nil
	}
	for _, path := range []string{
		instance.ServerDir(dir),
		instance.StagedServerDir(dir),
		instance.ServerDir(dir) + backup.SupersededSuffix,
		filepath.Join(dir, "logs"),
		instance.UpdateStaging(dir),
		instance.ParkedModsDir(dir),
	} {
		if err := d.removeInstanceFiles(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func (d Deleter) removeInstanceFiles(path string) error {
	removeAll := d.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	if err := removeAll(path); err != nil {
		return fmt.Errorf("remove tree: %w", err)
	}
	return nil
}

func deleteWithinRoot(root, path string) bool {
	within, err := filepath.Rel(root, path)
	return err == nil && within != "." && within != ".." &&
		!strings.HasPrefix(within, ".."+string(filepath.Separator))
}
