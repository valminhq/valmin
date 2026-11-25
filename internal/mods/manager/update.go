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
// the saved setup name, recording it as caused by requestedBy and using scratch as a work
// directory, and returns the finish-transaction step that records what it saved, nil when
// nothing was.
type Checkpoint func(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, name, requestedBy, scratch string,
) (func(context.Context, *sql.Tx) error, error)

// Version saves an instance's setup, without its world, before a change moves any mod files, as
// Checkpoint does. A setup it cannot save is logged on h, never an error.
type Version func(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, name, requestedBy, scratch string,
) func(context.Context, *sql.Tx) error

// checkpointBeforeUpdate saves the world and setup before an update moves any mod files.
func checkpointBeforeUpdate(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, checkpoint Checkpoint,
	pkgs []*stagedPackage, requestedBy, stagingDir string,
) (func(context.Context, *sql.Tx) error, error) {
	if checkpoint == nil {
		return nil, errors.New("this panel cannot archive worlds, so it will not update mods without a backup")
	}
	h.Progress(ctx, 64, "backing up the world")
	record, err := checkpoint(ctx, h, inst, checkpointName(pkgs), requestedBy, filepath.Join(stagingDir, "checkpoint"))
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating mods: %w", err)
	}
	return record, nil
}

// versionBefore saves the setup before a change named name moves any mod files, showing progress
// as the job's, and returns the finish step that records it, nil when version is nil or saved
// nothing.
func versionBefore(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, version Version, progress int,
	name, requestedBy, stagingDir string,
) func(context.Context, *sql.Tx) error {
	if version == nil {
		return nil
	}
	h.Progress(ctx, progress, "saving the current setup")
	return version(ctx, h, inst, name, requestedBy, filepath.Join(stagingDir, "version"))
}

// maxCheckpointNamed caps how many packages a checkpoint's name lists.
const maxCheckpointNamed = 2

// checkpointName names the setup saved before pkgs change: the packages moving to another
// version, else the packages being added, requested ones before dependencies, else a general
// name.
func checkpointName(pkgs []*stagedPackage) string {
	var updated, added, deps []string
	for _, p := range pkgs {
		switch {
		case p.prev != nil && !p.remove:
			updated = append(updated, p.fullName)
		case p.prev == nil && p.transitive:
			deps = append(deps, p.fullName)
		case p.prev == nil:
			added = append(added, p.fullName)
		}
	}
	added = append(added, deps...)
	switch {
	case len(updated) > 0:
		return beforeName("updating", updated)
	case len(added) > 0:
		return beforeName("installing", added)
	default:
		return "Before changing mods"
	}
}

// beforeName is "Before <verb> <names>", listing at most maxCheckpointNamed names.
func beforeName(verb string, names []string) string {
	switch n := len(names); {
	case n <= maxCheckpointNamed:
		return "Before " + verb + " " + strings.Join(names, " and ")
	default:
		return fmt.Sprintf("Before %s %s and %d more", verb, strings.Join(names[:maxCheckpointNamed], ", "),
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
