package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Deleter removes an instance's container and disposable files before deleting its row.
type Deleter struct {
	Engine   *jobs.Engine
	Runtime  runtime.Runtime
	DataRoot string
	// StopTimeout bounds the graceful stop of a container that is still running, which an
	// instance in error can have.
	StopTimeout time.Duration
}

// Run is idempotent so recovery can submit another delete after an interruption.
func (d *Deleter) Run(instanceID, containerID, dataDir string, keepWorlds bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		if containerID != "" {
			if err := d.stopIfRunning(ctx, jh, containerID); err != nil {
				return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
			}
		}
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

// stopIfRunning stops a running container with SIGINT so the game saves before the forced
// remove, which would otherwise kill it mid-write.
func (d *Deleter) stopIfRunning(ctx context.Context, jh *jobs.Handle, containerID string) error {
	c, err := d.Runtime.Inspect(ctx, containerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}
	if !c.Running {
		return nil
	}
	jh.Progress(ctx, 5, "stopping server")
	if err := d.Runtime.Stop(ctx, containerID, "SIGINT", d.StopTimeout); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (d *Deleter) deleteInstanceFiles(instanceID, dataDir string, keepWorlds bool) error {
	root := instance.InstancesDir(d.DataRoot)
	dir := filepath.Clean(dataDir)
	if !fsutil.Within(root, dir) {
		return fmt.Errorf("refusing path outside %s", root)
	}
	backupRoot := instance.BackupsDir(d.DataRoot)
	backupDir := filepath.Join(backupRoot, instanceID)
	if !fsutil.Within(backupRoot, backupDir) {
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

func (d *Deleter) removeInstanceFiles(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove tree: %w", err)
	}
	return nil
}
