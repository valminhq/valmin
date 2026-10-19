package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// rollbackInstall undoes a failed install from the manifests written before any file moved, the
// same way the crash sweep does. Every package is attempted even after one fails, and only those
// that came back cleanly have their rows removed: a row whose files remain is their only
// record.
func (i *Installer) rollbackInstall(
	ctx context.Context, inst *store.Instance, payload *InstallPayload,
	pkgs []*stagedPackage, cause error,
) jobs.Outcome {
	serverRoot := instance.ServerDir(inst.DataDir)
	backupDir := stagingBackupDir(payload.StagingDir)

	rolled := make([]string, 0, len(pkgs))
	var restore []store.InstanceMod
	var stuck []string
	for _, p := range pkgs {
		if err := installer.Rollback(
			rollbackEntries(p.manifest, p.prevStale), serverRoot, backupDir); err != nil {
			slog.ErrorContext(ctx, "mod install rollback incomplete",
				slog.String("instance_id", inst.ID), slog.String("full_name", p.fullName),
				slog.Any("error", err))
			stuck = append(stuck, p.fullName)
			continue
		}
		if p.prev != nil {
			// An update's files are back at the version this row describes, so the row goes
			// back with them rather than being deleted along with the fresh installs.
			restore = append(restore, *p.prev)
			continue
		}
		rolled = append(rolled, p.fullName)
	}
	if err := i.DB.RollbackInstanceMods(ctx, inst.ID, restore, rolled); err != nil {
		return jobs.Failed(errcode.Internal,
			fmt.Errorf("%w; and removing its rows failed: %w", cause, err))
	}
	if len(stuck) > 0 {
		// An install that failed is ordinary; one that could not be undone is not, and the
		// operator has to be told which packages still have files on disk.
		return jobs.Failed(errcode.Internal,
			fmt.Errorf("%w; and these could not be rolled back: %s", cause, strings.Join(stuck, ", ")))
	}
	return jobs.Failed(errcode.Internal, cause)
}
