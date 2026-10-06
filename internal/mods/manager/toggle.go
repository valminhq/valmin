package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

func toggleFailed(err error) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
}

// runModToggle is the mod_toggle Runner: move the files, then flip the row in the Finish
// transaction. A failure part-way settles every file back to where the unchanged row says it is.
func RunToggle(db *store.DB, inst *store.Instance, payload TogglePayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		row, manifest, err := toggleRow(ctx, db, inst.ID, payload.FullName)
		if err != nil {
			return toggleFailed(err)
		}
		if row.Enabled == payload.Enable {
			h.Progress(ctx, 100, "already in that state; nothing to do")
			return jobs.Outcome{Status: jobs.StatusSucceeded}
		}
		next, err := moveToggledFiles(ctx, h, inst, payload, manifest)
		if err != nil {
			return settleToggle(ctx, inst, payload.FullName, manifest, err)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return settleToggle(ctx, inst, payload.FullName, manifest, err)
		}
		return finishToggle(ctx, h, inst, payload, string(raw))
	}
}

// moveToggledFiles does the one move a toggle is, and returns the manifest that records it.
func moveToggledFiles(
	ctx context.Context, h *jobs.Handle, inst *store.Instance,
	payload TogglePayload, manifest []installer.ManifestEntry,
) ([]installer.ManifestEntry, error) {
	serverRoot, parkDir := serverDir(inst), parkedPackageDir(inst, payload.FullName)
	if payload.Enable {
		h.Progress(ctx, 30, "putting the mod's files back")
		if err := installer.Unpark(installer.ParkedPaths(manifest), parkDir, serverRoot); err != nil {
			return nil, fmt.Errorf("enable %s: %w", payload.FullName, err)
		}
		return installer.MarkParked(manifest, nil), nil
	}
	h.Progress(ctx, 30, "moving the mod's files out of the server")
	moved, err := installer.Park(installer.Movable(manifest), serverRoot, parkDir)
	if err != nil {
		return nil, fmt.Errorf("disable %s: %w", payload.FullName, err)
	}
	h.Log(fmt.Sprintf("%s: %d files moved out of the server", payload.FullName, len(moved)))
	return installer.MarkParked(manifest, moved), nil
}

// finishToggle is the successful outcome: the row and restart_required in the Finish
// transaction, and for an enable, the emptied parking directory once they have committed.
func finishToggle(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, payload TogglePayload, manifest string,
) jobs.Outcome {
	verb := "disabled"
	if payload.Enable {
		verb = "enabled"
	}
	h.Progress(ctx, 100, fmt.Sprintf("%s %s", verb, payload.FullName))
	out := jobs.Outcome{
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxSetInstanceModEnabled(
				ctx, tx, inst.ID, payload.FullName, payload.Enable, manifest); err != nil {
				return fmt.Errorf("record the toggle: %w", err)
			}
			if err := store.TxSetRestartRequired(ctx, tx, inst.ID); err != nil {
				return fmt.Errorf("record the toggle: %w", err)
			}
			return nil
		},
	}
	if payload.Enable {
		// Only empty directories remain once every parked file is back; removed after the row
		// commits, so a failed Finish still finds the tree the sweep settles against.
		parkDir := parkedPackageDir(inst, payload.FullName)
		out.AfterFinish = func(context.Context) { _ = os.RemoveAll(parkDir) }
	}
	return out
}

// toggleRow reads the package's row and decodes its manifest.
func toggleRow(
	ctx context.Context, db *store.DB, instanceID, fullName string,
) (*store.InstanceMod, []installer.ManifestEntry, error) {
	rows, err := db.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read installed mods: %w", err)
	}
	for i := range rows {
		if rows[i].FullName != fullName {
			continue
		}
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(rows[i].FileManifest), &manifest); err != nil {
			return nil, nil, fmt.Errorf("read the manifest of %s: %w", fullName, err)
		}
		return &rows[i], manifest, nil
	}
	return nil, nil, fmt.Errorf("%s is no longer installed", fullName)
}

// settleToggle is a failed toggle's undo: every file goes back to where the manifest, which the
// job never changed, records it.
func settleToggle(
	ctx context.Context, inst *store.Instance, fullName string,
	manifest []installer.ManifestEntry, cause error,
) jobs.Outcome {
	if err := installer.Settle(manifest, serverDir(inst), parkedPackageDir(inst, fullName)); err != nil {
		slog.ErrorContext(ctx, "mod toggle undo incomplete",
			slog.String("instance_id", inst.ID), slog.String("full_name", fullName),
			slog.Any("error", err))
		return toggleFailed(
			fmt.Errorf("%w; and these files could not be put back: %w", cause, err))
	}
	return toggleFailed(cause)
}
