package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// ArchiveWorlds saves an instance's world and returns its finish-transaction record.
type ArchiveWorlds func(context.Context, *store.Instance, string) (func(context.Context, *sql.Tx) error, error)

// archiveBeforeUpdate saves the world before an update moves any mod files.
func archiveBeforeUpdate(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, archive ArchiveWorlds,
) (func(context.Context, *sql.Tx) error, error) {
	if archive == nil {
		return nil, errors.New("this panel cannot archive worlds, so it will not update mods without a backup")
	}
	h.Progress(ctx, 64, "backing up the world")
	record, err := archive(ctx, inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating mods: %w", err)
	}
	if record == nil {
		h.Log("no world archive was taken: this server has no world yet")
	}
	return record, nil
}

// updateSummary names the confirmed version changes in a job log entry.
func updateSummary(targets []UpdateTarget) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s %s -> %s", t.FullName, t.FromVersion, t.Version))
	}
	return "updating " + strings.Join(parts, ", ")
}
