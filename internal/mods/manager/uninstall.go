package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

const (
	CheckpointSaved   = "saved"
	CheckpointRemoved = "removed"
	BepInExPack       = "denikson-BepInExPack_Valheim"
)

// removedPackage is one package of the removal set, with the manifest that defines it.
type removedPackage struct {
	fullName string
	manifest []installer.ManifestEntry
}

// runUninstall is the mod_uninstall Runner: save every file packageGroups gives it, remove
// them, and delete the rows last, in the job's own Finish transaction. That order is what makes a
// crash benign: the rows still describe the missing files and the backups can restore them.
func runUninstall(db *store.DB, inst *store.Instance, payload UninstallPayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()

		pkgs, err := removalManifests(ctx, db, inst.ID, payload.FullNames)
		if err != nil {
			return modJobFailed(errcode.Internal, err)
		}
		backupDir := stagingBackupDir(payload.StagingDir)
		configs := configPaths(payload.Configs)

		h.Progress(ctx, 20, fmt.Sprintf("saving the files of %d packages", len(pkgs)))
		if err := saveRemovals(inst, pkgs, configs, backupDir); err != nil {
			// Nothing has been removed, so there is nothing to put back.
			return modJobFailed(errcode.Internal, err)
		}
		if err := h.Checkpoint(ctx, CheckpointSaved); err != nil {
			return modJobFailed(errcode.Internal, err)
		}

		h.Progress(ctx, 60, "removing files")
		for _, p := range pkgs {
			removed, err := removePackage(inst, p)
			if err != nil {
				return rollbackUninstall(ctx, inst, pkgs, configs, backupDir, err)
			}
			h.Log(fmt.Sprintf("%s: %d files removed", p.fullName, removed))
		}
		if err := installer.Remove(configs, serverDir(inst)); err != nil {
			return rollbackUninstall(ctx, inst, pkgs, configs, backupDir, fmt.Errorf("remove config files: %w", err))
		}
		if len(payload.Configs) > 0 {
			h.Log("config files removed: " + strings.Join(payload.Configs, ", "))
		}
		if err := h.Checkpoint(ctx, CheckpointRemoved); err != nil {
			return rollbackUninstall(ctx, inst, pkgs, configs, backupDir, err)
		}

		h.Progress(ctx, 100, fmt.Sprintf("removed %d packages", len(pkgs)))
		return jobs.Outcome{
			Status:   jobs.StatusSucceeded,
			OnFinish: finishUninstall(inst.ID, payload.FullNames),
			// A disabled package's parking directory holds nothing its row names once the row is
			// gone, only the directories its files were in.
			AfterFinish: func(context.Context) {
				for _, name := range payload.FullNames {
					_ = os.RemoveAll(parkedPackageDir(inst, name))
				}
			},
		}
	}
}

// saveRemovals copies every file the removal set will remove, from whichever tree it is in, and
// the config files removed with it into the job's backup directory before anything is removed.
func saveRemovals(inst *store.Instance, pkgs []removedPackage, configs []string, backupDir string) error {
	for _, p := range pkgs {
		for _, g := range packageGroups(inst, p.fullName, p.manifest) {
			if err := installer.BackupPaths(installer.Paths(g.manifest), g.root, backupDir); err != nil {
				return fmt.Errorf("save %s: %w", p.fullName, err)
			}
		}
	}
	if err := installer.BackupPaths(configs, serverDir(inst), backupDir); err != nil {
		return fmt.Errorf("save config files: %w", err)
	}
	return nil
}

// configPaths is the server-relative paths of config files and of the copies the panel keeps of
// each.
func configPaths(files []string) []string {
	suffixes := []string{"", modconfig.BackupSuffix, modconfig.OriginalSuffix, modconfig.PendingSuffix}
	paths := make([]string, 0, len(files)*len(suffixes))
	for _, f := range files {
		for _, suffix := range suffixes {
			paths = append(paths, path.Join(instance.ConfigDir, f+suffix))
		}
	}
	return paths
}

// savedCopies is the paths the backup directory holds a copy of, as entries Rollback restores.
// A path without a copy was never removed and is left as it is.
func savedCopies(paths []string, backupDir string) []installer.ManifestEntry {
	var saved []installer.ManifestEntry
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(backupDir, filepath.FromSlash(p))); err == nil {
			saved = append(saved, installer.ManifestEntry{Path: p})
		}
	}
	return saved
}

// removePackage removes one package's files from both trees and reports how many paths it
// removed.
func removePackage(inst *store.Instance, p removedPackage) (int, error) {
	removed := 0
	for _, g := range packageGroups(inst, p.fullName, p.manifest) {
		if err := installer.Remove(installer.Paths(g.manifest), g.root); err != nil {
			return removed, fmt.Errorf("remove %s: %w", p.fullName, err)
		}
		removed += len(g.manifest)
	}
	return removed, nil
}

// removalManifests reads the manifest of every package in the removal set. A row missing since
// the request stops the job: the manifest is the only exact record of that package's files, and
// removing one without it means re-running the placement heuristics (B9).
func removalManifests(
	ctx context.Context, db *store.DB, instanceID string, fullNames []string,
) ([]removedPackage, error) {
	rows, err := db.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	byName := make(map[string]string, len(rows))
	for i := range rows {
		byName[rows[i].FullName] = rows[i].FileManifest
	}

	pkgs := make([]removedPackage, 0, len(fullNames))
	for _, name := range fullNames {
		raw, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("%s is no longer installed", name)
		}
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
			return nil, fmt.Errorf("read the manifest of %s: %w", name, err)
		}
		pkgs = append(pkgs, removedPackage{fullName: name, manifest: manifest})
	}
	return pkgs, nil
}

// rollbackUninstall puts back everything the job saved. Every package is attempted even
// after one fails, and what could not be restored is named — an uninstall that failed is
// ordinary, one that left the server in neither state is not.
func rollbackUninstall(
	ctx context.Context, inst *store.Instance, pkgs []removedPackage, configs []string, backupDir string,
	cause error,
) jobs.Outcome {
	var stuck []string
	if saved := savedCopies(configs, backupDir); len(saved) > 0 {
		if err := installer.Rollback(saved, serverDir(inst), backupDir); err != nil {
			slog.ErrorContext(ctx, "mod uninstall rollback incomplete: config files",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
			stuck = append(stuck, "config files")
		}
	}
	for _, p := range pkgs {
		for _, g := range packageGroups(inst, p.fullName, p.manifest) {
			if err := installer.Rollback(g.manifest, g.root, backupDir); err != nil {
				slog.ErrorContext(ctx, "mod uninstall rollback incomplete",
					slog.String("instance_id", inst.ID), slog.String("full_name", p.fullName),
					slog.Any("error", err))
				stuck = append(stuck, p.fullName)
				break
			}
		}
	}
	if len(stuck) > 0 {
		return modJobFailed(errcode.Internal,
			fmt.Errorf("%w; and these could not be put back: %s", cause, strings.Join(stuck, ", ")))
	}
	return modJobFailed(errcode.Internal, cause)
}

// finishUninstall is the state flip (12 §6): the rows go, the instance is marked as needing
// a restart, and an instance that has just lost BepInEx stops being a modded one.
func finishUninstall(instanceID string, fullNames []string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxDeleteInstanceMods(ctx, tx, instanceID, fullNames); err != nil {
			return fmt.Errorf("remove the rows of an uninstall: %w", err)
		}
		if err := store.TxSetPendingRestart(ctx, tx, instanceID); err != nil {
			return fmt.Errorf("mark %s as needing a restart: %w", instanceID, err)
		}
		for _, name := range fullNames {
			if name == BepInExPack {
				return store.TxClearModded(ctx, tx, instanceID)
			}
		}
		return nil
	}
}
