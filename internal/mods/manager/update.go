package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Checkpoint saves an instance's world and setup before an update moves any mod files, naming
// the saved setup name and using scratch as a work directory, and returns the
// finish-transaction step that records what it saved, nil when nothing was.
type Checkpoint func(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, name, scratch string,
) (func(context.Context, *sql.Tx) error, error)

// checkpointBeforeUpdate saves the world and setup before an update moves any mod files.
func checkpointBeforeUpdate(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, checkpoint Checkpoint,
	pkgs []*stagedPackage, stagingDir string,
) (func(context.Context, *sql.Tx) error, error) {
	if checkpoint == nil {
		return nil, errors.New("this panel cannot archive worlds, so it will not update mods without a backup")
	}
	h.Progress(ctx, 64, "backing up the world")
	record, err := checkpoint(ctx, h, inst, checkpointName(pkgs), filepath.Join(stagingDir, "checkpoint"))
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating mods: %w", err)
	}
	return record, nil
}

// maxCheckpointNamed caps how many updated packages a checkpoint's name lists.
const maxCheckpointNamed = 2

// checkpointName names the setup saved before pkgs change: the packages moving to another
// version, or a general name for a change that only adds or removes packages.
func checkpointName(pkgs []*stagedPackage) string {
	var updated []string
	for _, p := range pkgs {
		if p.prev != nil && !p.remove {
			updated = append(updated, p.fullName)
		}
	}
	switch n := len(updated); {
	case n == 0:
		return "Before changing mods"
	case n <= maxCheckpointNamed:
		return "Before updating " + strings.Join(updated, " and ")
	default:
		return fmt.Sprintf("Before updating %s and %d more", strings.Join(updated[:maxCheckpointNamed], ", "),
			n-maxCheckpointNamed)
	}
}

// updateSummary names the confirmed version changes in a job log entry.
func updateSummary(targets []UpdateTarget) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s %s -> %s", t.FullName, t.FromVersion, t.Version))
	}
	return "updating " + strings.Join(parts, ", ")
}
