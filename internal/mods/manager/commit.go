package manager

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// commitInstall is the half that changes things: back up what the whole closure would displace,
// record the manifests, then move the files.
//
// The backup pass covers every package before the first manifest row is written, because
// rollback reads "a manifest path with no backup" as its own to delete. The cancellation check
// here is the job's last: past the manifests, the rollback path owns the outcome.
func (i *Installer) commitInstall(
	ctx context.Context, h *jobs.Handle, inst *store.Instance,
	payload *InstallPayload, pkgs []*stagedPackage,
) jobs.Outcome {
	if h.CancelRequested(ctx) {
		return jobs.Outcome{Status: jobs.StatusCancelled}
	}

	serverRoot := instance.ServerDir(inst.DataDir)
	backupDir := stagingBackupDir(payload.StagingDir)

	h.Progress(ctx, 68, "saving what would be replaced")
	for _, p := range pkgs {
		if err := installer.Backup(p.changes, serverRoot, backupDir); err != nil {
			// Nothing is recorded and nothing has moved, so there is nothing to undo.
			return jobs.Failed(errcode.Internal, fmt.Errorf("back up for %s: %w", p.fullName, err))
		}
		// An update's stale files are displaced too, so they are saved in the same pass and
		// before the same checkpoint: rollback's only rule is "restore what has a backup".
		if err := installer.BackupPaths(p.prevStale, serverRoot, backupDir); err != nil {
			return jobs.Failed(errcode.Internal,
				fmt.Errorf("back up what %s replaces: %w", p.fullName, err))
		}
	}
	if err := writePrevRows(payload.StagingDir, pkgs); err != nil {
		return jobs.Failed(errcode.Internal, err)
	}
	// The crash sweep undoes nothing before this checkpoint: no row has changed and no file
	// has moved.
	if err := h.Checkpoint(ctx, CheckpointBackedUp); err != nil {
		return jobs.Failed(errcode.Internal, err)
	}

	h.Progress(ctx, 70, "recording the file manifest")
	if err := i.writeManifests(ctx, inst.ID, pkgs); err != nil {
		return jobs.Failed(errcode.Internal, err)
	}

	// From here every failure goes through the rollback, including a checkpoint that will
	// not write: the rows and pending_restart are committed, so a job ending terminal
	// without undoing them leaves an install the sweep never revisits.
	if err := h.Checkpoint(ctx, CheckpointManifestWritten); err != nil {
		return i.rollbackInstall(ctx, inst, payload, pkgs, err)
	}

	h.Progress(ctx, 85, "placing files")
	for _, p := range pkgs {
		// The old version's files come off first, from its own manifest (B9), so what the
		// new version does not ship cannot survive as an orphan BepInEx would still load.
		if err := installer.Remove(p.prevStale, serverRoot); err != nil {
			return i.rollbackInstall(ctx, inst, payload, pkgs,
				fmt.Errorf("remove the replaced files of %s: %w", p.fullName, err))
		}
		if err := installer.Apply(p.changes, serverRoot); err != nil {
			return i.rollbackInstall(ctx, inst, payload, pkgs, fmt.Errorf("apply %s: %w", p.fullName, err))
		}
	}
	if err := h.Checkpoint(ctx, CheckpointApplied); err != nil {
		return i.rollbackInstall(ctx, inst, payload, pkgs, err)
	}

	h.Progress(ctx, 100, fmt.Sprintf("installed %d packages", len(pkgs)))
	return jobs.Outcome{
		Status:   jobs.StatusSucceeded,
		OnFinish: finishInstall(inst.ID, i.installedBepInEx(ctx, inst, pkgs), removedNames(pkgs)),
		// The console key is flipped only after the install commits. It is in no manifest,
		// because an install never overwrites an existing config, so a crash between the
		// edit and the commit would undo every file and leave the edit standing.
		AfterFinish: func(ctx context.Context) {
			i.ensureConsoleLogging(ctx, serverRoot, pkgs)
			i.ensureRCON(ctx, inst, pkgs)
		},
	}
}

func (i *Installer) ensureRCON(ctx context.Context, inst *store.Instance, pkgs []*stagedPackage) {
	if i.Commands == nil || versionOf(pkgs, command.ValheimRCONPackage) == "" {
		return
	}
	if err := i.Commands.Configure(ctx, inst.ID, inst.DataDir); err != nil {
		slog.WarnContext(ctx, "RCON configuration failed",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
	}
}

// installedBepInEx is the framework version this instance ends up running, or "" if it is not
// modded. It falls back to the installed row, since a package already present at a satisfying
// version never appears in pkgs and the instance would stay unflagged.
func (i *Installer) installedBepInEx(ctx context.Context, inst *store.Instance, pkgs []*stagedPackage) string {
	if version := versionOf(pkgs, BepInExPack); version != "" {
		return version
	}
	if inst.Modded {
		return ""
	}
	version, _, ok, err := i.DB.InstanceModVersion(ctx, inst.ID, BepInExPack)
	if err != nil || !ok {
		return ""
	}
	return version
}

// ensureConsoleLogging turns BepInEx's console logging on, only when this install placed
// the framework package. A file it cannot change is a warning, never a failure: the server
// runs fine, the panel just cannot read its plugin lines.
func (i *Installer) ensureConsoleLogging(ctx context.Context, serverRoot string, pkgs []*stagedPackage) {
	if versionOf(pkgs, BepInExPack) == "" {
		return
	}
	path := filepath.Join(serverRoot, filepath.FromSlash(bepinexConfig))
	changed, err := modconfig.EnsureConsoleLogging(path)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "bepinex console logging unconfirmed; this server may load its "+
			"plugins without the panel being able to see it",
			slog.String("path", path), slog.Any("error", err))
	case changed:
		slog.InfoContext(ctx, "turned on BepInEx console logging so plugin loading is visible",
			slog.String("path", path))
	}
}

// finishInstall records that this instance now runs BepInEx, and deletes the rows of the
// packages the install removed. It lands in the job's Finish transaction from data already in
// memory, so it is written only once the files are settled and never survives a rollback.
func finishInstall(instanceID, bepinex string, removed []string) func(context.Context, *sql.Tx) error {
	if bepinex == "" && len(removed) == 0 {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if bepinex != "" {
			if err := store.TxSetModded(ctx, tx, instanceID, bepinex); err != nil {
				return fmt.Errorf("record the framework version: %w", err)
			}
		}
		if err := store.TxDeleteInstanceMods(ctx, tx, instanceID, removed); err != nil {
			return fmt.Errorf("delete the removed mods' rows: %w", err)
		}
		return nil
	}
}

// removedNames is the packages an install uninstalls.
func removedNames(pkgs []*stagedPackage) []string {
	var out []string
	for _, p := range pkgs {
		if p.remove {
			out = append(out, p.fullName)
		}
	}
	return out
}

// versionOf is the version an install places of fullName, or "" when it places none.
func versionOf(pkgs []*stagedPackage, fullName string) string {
	for _, p := range pkgs {
		if p.fullName == fullName && !p.remove {
			return p.version
		}
	}
	return ""
}

// writeManifests records every package's rows and marks the instance as needing a restart,
// in one transaction. The state flip is transactional; the work that produced it was not. A
// removed package keeps its row with an empty manifest until the Finish transaction deletes it,
// so the crash sweep reads it back as nothing to undo but its stale files.
func (i *Installer) writeManifests(ctx context.Context, instanceID string, pkgs []*stagedPackage) error {
	rows := make([]store.InstanceMod, 0, len(pkgs))
	for _, p := range pkgs {
		installedAs := store.InstalledExplicit
		if p.transitive {
			installedAs = store.InstalledDependency
		}
		rows = append(rows, store.InstanceMod{
			InstanceID: instanceID, FullName: p.fullName, Source: p.src, Version: p.version,
			InstalledAs: installedAs, Side: store.SideUnknown, Enabled: true,
			FileManifest: p.manifestRaw,
		})
	}
	if err := i.DB.WriteInstanceMods(ctx, instanceID, rows); err != nil {
		return fmt.Errorf("record the file manifests: %w", err)
	}
	return nil
}
