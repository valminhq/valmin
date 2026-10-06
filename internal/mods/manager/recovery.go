package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// Recovery settles interrupted mod file operations from their persisted payloads.
type Recovery struct {
	DB       *store.DB
	DataRoot string
}

func stagingRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "staging", "mods")
}

func stagingBackupDir(stagingDir string) string { return filepath.Join(stagingDir, "backup") }

func parkedPackageDir(inst *store.Instance, fullName string) string {
	return filepath.Join(instance.ParkedModsDir(inst.DataDir), fullName)
}

type fileGroup struct {
	root     string
	manifest []installer.ManifestEntry
}

// packageGroups is the part of a package's manifest an uninstall owns, by tree. A file under
// BepInEx/config/ is left out: it holds the admin's settings, so an uninstall never saves,
// removes or restores it, the same line an update and a disable draw (B10).
func packageGroups(inst *store.Instance, fullName string, manifest []installer.ManifestEntry) []fileGroup {
	owned := make([]installer.ManifestEntry, 0, len(manifest))
	for _, e := range manifest {
		if !installer.UserConfig(e.Path) {
			owned = append(owned, e)
		}
	}
	inServer, parked := installer.Split(owned)
	return []fileGroup{
		{root: serverDir(inst), manifest: inServer},
		{root: parkedPackageDir(inst, fullName), manifest: parked},
	}
}

func prevRowDir(stagingDir string) string { return filepath.Join(stagingDir, "prev") }

func prevRowPath(stagingDir, fullName string) string {
	return filepath.Join(prevRowDir(stagingDir), fullName+".json")
}

// replaced is what an update saves about the version it overwrites: the row, and the exact set
// of files it is about to remove. Stale is recorded rather than re-derived, since "old manifest
// minus new" would include the config files the diff deliberately skipped.
type replaced struct {
	Row   store.InstanceMod `json:"row"`
	Stale []string          `json:"stale"`
}

func readPrevRow(stagingDir, fullName string) (*replaced, error) {
	// fullName passed installer.CheckFullName before this job ever used it as a path.
	path := prevRowPath(stagingDir, fullName)
	raw, err := os.ReadFile(path) //nolint:gosec // see above
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the replaced row for %s: %w", fullName, err)
	}
	var prev replaced
	if err := json.Unmarshal(raw, &prev); err != nil {
		return nil, fmt.Errorf("decode the replaced row for %s: %w", fullName, err)
	}
	return &prev, nil
}

func rollbackEntries(manifest []installer.ManifestEntry, stale []string) []installer.ManifestEntry {
	out := make([]installer.ManifestEntry, 0, len(manifest)+len(stale))
	out = append(out, manifest...)
	for _, path := range stale {
		out = append(out, installer.ManifestEntry{Path: path})
	}
	return out
}

func serverDir(inst *store.Instance) string { return filepath.Join(inst.DataDir, "server") }

func (s *Recovery) SweepModInstall(ctx context.Context, j *store.Job) {
	var payload InstallPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted mod install: payload unreadable, nothing rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	root := stagingRoot(s.DataRoot)
	if payload.StagingDir == "" || !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(
			ctx,
			"interrupted mod install names a staging directory outside the staging root; not touching it",
			slog.String("job_id", j.ID),
			slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root),
		)
		return
	}
	defer func() {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "interrupted mod install: staging directory not removed",
				slog.String("job_id", j.ID), slog.Any("error", err))
		}
	}()

	// Rows change and files move only after the backed_up checkpoint, so a job stopped before
	// it left nothing to undo.
	if j.InstanceID == nil || !installRecorded(j.Checkpoint) {
		return
	}
	inst, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "interrupted mod install: instance unreadable, nothing rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}

	restore, rolled := s.rollbackStaged(ctx, j, inst, payload.StagingDir)
	if err := s.DB.RollbackInstanceMods(ctx, inst.ID, restore, rolled); err != nil {
		slog.ErrorContext(ctx, "interrupted mod install: rows not restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	if len(rolled)+len(restore) > 0 {
		slog.InfoContext(ctx, "rolled back an interrupted mod install",
			slog.String("job_id", j.ID), slog.Int("packages", len(rolled)+len(restore)))
	}
}

// installRecorded reports whether an interrupted mod_install got as far as changing rows.
func installRecorded(checkpoint *string) bool {
	if checkpoint == nil {
		return false
	}
	switch *checkpoint {
	case CheckpointBackedUp, CheckpointManifestWritten, CheckpointApplied:
		return true
	}
	return false
}

// SweepModToggle settles an interrupted disable or enable. The row is written only in the job's
// Finish transaction, so it still records where every file was before the job; each file is
// returned there from whichever tree the interruption left it in (Q37).
func (s *Recovery) SweepModToggle(ctx context.Context, j *store.Job) {
	var payload TogglePayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil || j.InstanceID == nil {
		slog.WarnContext(ctx, "interrupted mod toggle: payload unreadable, nothing settled",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	inst, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "interrupted mod toggle: instance unreadable, nothing settled",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	_, manifest, err := toggleRow(ctx, s.DB, inst.ID, payload.FullName)
	if err != nil {
		slog.WarnContext(ctx, "interrupted mod toggle: row unreadable, nothing settled",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	if err := installer.Settle(
		manifest, serverDir(inst), parkedPackageDir(inst, payload.FullName)); err != nil {
		slog.ErrorContext(ctx, "interrupted mod toggle: files not fully settled",
			slog.String("job_id", j.ID), slog.String("full_name", payload.FullName),
			slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "settled the files of an interrupted mod toggle",
		slog.String("job_id", j.ID), slog.String("full_name", payload.FullName))
}

// restoreRemoval puts one package of an interrupted uninstall back, per tree, with the same call
// the job's own failure path makes: a manifest path with a saved copy goes back, and one without
// was already gone before the uninstall began. A config file is outside packageGroups and goes
// back only when the backup holds a copy, which an uninstall interrupted under an earlier build
// may have saved and removed; one without a copy is the admin's and is left alone.
func restoreRemoval(
	ctx context.Context, j *store.Job, inst *store.Instance, name string,
	manifest []installer.ManifestEntry, backupDir string,
) bool {
	var saved []installer.ManifestEntry
	for _, e := range manifest {
		if !installer.UserConfig(e.Path) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(backupDir, filepath.FromSlash(e.Path))); err == nil {
			saved = append(saved, e)
		}
	}
	ok := true
	groups := append(packageGroups(inst, name, manifest), fileGroup{root: serverDir(inst), manifest: saved})
	for _, g := range groups {
		if err := installer.Rollback(g.manifest, g.root, backupDir); err != nil {
			slog.ErrorContext(ctx, "interrupted mod uninstall: files not fully restored",
				slog.String("job_id", j.ID), slog.String("full_name", name), slog.Any("error", err))
			ok = false
		}
	}
	return ok
}

// SweepModUninstall rolls an interrupted mod_uninstall back by restoring the files it saved.
// The job backs up every file before removing any and deletes its rows only in its own Finish
// transaction, so an interrupted one still has them.
func (s *Recovery) SweepModUninstall(ctx context.Context, j *store.Job) {
	var payload UninstallPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted mod uninstall: payload unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	root := stagingRoot(s.DataRoot)
	if payload.StagingDir == "" || !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(
			ctx,
			"interrupted mod uninstall names a staging directory outside the staging root; not touching it",
			slog.String("job_id", j.ID),
			slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root),
		)
		return
	}
	defer func() {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "interrupted mod uninstall: staging directory not removed",
				slog.String("job_id", j.ID), slog.Any("error", err))
		}
	}()

	if j.InstanceID == nil {
		return
	}
	inst, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "interrupted mod uninstall: instance unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	installed, err := s.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted mod uninstall: installed mods unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	byName := make(map[string]string, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = installed[i].FileManifest
	}

	restored := 0
	for _, name := range payload.FullNames {
		manifest, ok := decodeManifest(ctx, j, name, byName)
		if !ok {
			continue
		}
		if restoreRemoval(ctx, j, inst, name, manifest, stagingBackupDir(payload.StagingDir)) {
			restored++
		}
	}
	if restored > 0 {
		slog.InfoContext(ctx, "restored the files of an interrupted mod uninstall",
			slog.String("job_id", j.ID), slog.Int("packages", restored))
	}
}

// decodeManifest reads one installed package's file manifest out of the rows read for the
// sweep. A package with no row was never recorded; a row whose manifest will not decode is
// reported and left alone, because guessing at its files is what B9 forbids.
func decodeManifest(
	ctx context.Context, j *store.Job, fullName string, byName map[string]string,
) ([]installer.ManifestEntry, bool) {
	raw, ok := byName[fullName]
	if !ok {
		return nil, false
	}
	var manifest []installer.ManifestEntry
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		slog.ErrorContext(ctx, "interrupted mod job: manifest unreadable, files left as they are",
			slog.String("job_id", j.ID), slog.String("full_name", fullName), slog.Any("error", err))
		return nil, false
	}
	return manifest, true
}

// rollbackStaged undoes every package the interrupted job had staged. It returns the rows to
// put back, being the versions an interrupted update had overwritten, and the names of the rows
// to delete. A package with no row never reached server/, since rows are written before any
// file moves.
func (s *Recovery) rollbackStaged(
	ctx context.Context, j *store.Job, inst *store.Instance, stagingDir string,
) (restore []store.InstanceMod, rolled []string) {
	staged, err := os.ReadDir(filepath.Join(stagingDir, "pkg"))
	if err != nil {
		// Killed before anything was staged, so nothing was written either.
		return nil, nil
	}
	installed, err := s.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted mod install: installed mods unreadable, not rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return nil, nil
	}
	byName := make(map[string]string, len(installed))
	versions := make(map[string]string, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = installed[i].FileManifest
		versions[installed[i].FullName] = installed[i].Version
	}

	serverRoot := filepath.Join(inst.DataDir, "server")
	backupDir := stagingBackupDir(stagingDir)
	for _, d := range staged {
		manifest, ok := decodeManifest(ctx, j, d.Name(), byName)
		if !ok {
			continue
		}
		// What the job had recorded, plus what an update had already removed to make room —
		// read from the staging directory, because from manifest_written onward the row
		// itself describes the new version and no longer names the old version's files.
		prev, err := readPrevRow(stagingDir, d.Name())
		if err != nil {
			slog.ErrorContext(ctx, "interrupted mod install: the replaced row is unreadable, files left in place",
				slog.String("job_id", j.ID), slog.String("full_name", d.Name()), slog.Any("error", err))
			continue
		}
		// A row still as the job found it was never rewritten, so nothing of it moved.
		if prev != nil && versions[d.Name()] == prev.Row.Version && byName[d.Name()] == prev.Row.FileManifest {
			continue
		}
		var stale []string
		if prev != nil {
			stale = prev.Stale
		}
		if err := installer.Rollback(
			rollbackEntries(manifest, stale), serverRoot, backupDir); err != nil {
			slog.ErrorContext(ctx, "interrupted mod install: rollback incomplete",
				slog.String("job_id", j.ID), slog.String("full_name", d.Name()), slog.Any("error", err))
			continue
		}
		if prev != nil {
			restore = append(restore, prev.Row)
			continue
		}
		rolled = append(rolled, d.Name())
	}
	return restore, rolled
}

// withinRoot reports whether path is inside root. The paths it guards come out of a
// database column and feed a recursive delete running as the panel, so a payload naming
// somewhere else entirely gets nothing removed rather than the benefit of the doubt.
func withinRoot(root, path string) bool {
	within, err := filepath.Rel(root, path)
	return err == nil && within != "." && within != ".." &&
		!strings.HasPrefix(within, ".."+string(filepath.Separator))
}
