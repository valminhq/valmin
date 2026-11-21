package manager

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path"
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
	before, modWrote, err := trackConfigs(ctx, i.DB, inst, replacedConfigs(pkgs), backupDir)
	if err != nil {
		return jobs.Failed(errcode.Internal, fmt.Errorf("record the config files the install replaces: %w", err))
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

	dropReplacedPending(ctx, h, serverRoot, pkgs)
	revs, err := installedConfigRevisions(inst.ID, pkgs, before, modWrote)
	if err != nil {
		h.Log(fmt.Sprintf("warning: the new config files are not in their history: %v", err))
	}

	h.Progress(ctx, 100, "Installed "+packageCount(len(pkgs)))
	out := jobs.Outcome{
		Status:   jobs.StatusSucceeded,
		OnFinish: finishInstall(inst.ID, i.installedBepInEx(ctx, inst, pkgs), removedNames(pkgs)),
		// The console key is flipped only after the install commits. The edit is in no manifest,
		// so a crash between it and the commit would undo every file and leave the edit standing.
		AfterFinish: func(ctx context.Context) {
			i.ensureConsoleLogging(ctx, serverRoot, pkgs)
			i.ensureRCON(ctx, inst, pkgs)
		},
	}
	return out.PrependFinish(recordInstalledConfigs(revs))
}

// dropReplacedPending removes the pending copy of every config file the install overwrote, so
// the next settle cannot lay an older panel edit over the mod's new version. A copy it cannot
// remove is a warning in the job log.
func dropReplacedPending(ctx context.Context, h *jobs.Handle, serverRoot string, pkgs []*stagedPackage) {
	root, err := os.OpenRoot(serverRoot)
	if err != nil {
		h.Log(fmt.Sprintf("warning: pending config edits not cleared: %v", err))
		return
	}
	defer func() { _ = root.Close() }()
	for _, p := range pkgs {
		for _, c := range p.changes {
			if c.Action != installer.ActionOverwrite || !installer.UserConfig(c.Dest) {
				continue
			}
			if err := modconfig.DropPending(root, filepath.FromSlash(c.Dest)); err != nil {
				h.Log(fmt.Sprintf("warning: pending config edit not cleared: %v", err))
				slog.WarnContext(ctx, "pending config edit not cleared after a mod update",
					slog.String("path", c.Dest), slog.Any("error", err))
			}
		}
	}
}

// wrote reports whether the install writes dest, in any package.
func wrote(pkgs []*stagedPackage, dest string) bool {
	for _, p := range pkgs {
		for _, c := range p.changes {
			if c.Dest == dest && c.Action != installer.ActionSkip {
				return true
			}
		}
	}
	return false
}

// ensureRCON configures the RCON plugin when this install placed it or wrote its config file.
func (i *Installer) ensureRCON(ctx context.Context, inst *store.Instance, pkgs []*stagedPackage) {
	placed := versionOf(pkgs, command.ValheimRCONPackage) != ""
	if i.Commands == nil || !placed && !wrote(pkgs, path.Join(instance.ConfigDir, command.ConfigFile)) {
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

// ensureConsoleLogging turns BepInEx's console logging on when this install placed the
// framework package or wrote its config file. A file it cannot change is a warning, never a
// failure: the server runs fine, the panel just cannot read its plugin lines.
func (i *Installer) ensureConsoleLogging(ctx context.Context, serverRoot string, pkgs []*stagedPackage) {
	if versionOf(pkgs, BepInExPack) == "" && !wrote(pkgs, bepinexConfig) {
		return
	}
	file := filepath.Join(serverRoot, filepath.FromSlash(bepinexConfig))
	changed, err := modconfig.EnsureConsoleLogging(file)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "bepinex console logging unconfirmed; this server may load its "+
			"plugins without the panel being able to see it",
			slog.String("path", file), slog.Any("error", err))
	case changed:
		slog.InfoContext(ctx, "turned on BepInEx console logging so plugin loading is visible",
			slog.String("path", file))
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

// packageCount is n packages in words, singular for one.
func packageCount(n int) string {
	if n == 1 {
		return "1 package"
	}
	return fmt.Sprintf("%d packages", n)
}
