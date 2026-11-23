package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// SetupPaths is every file a setup of mods and config files manages, by root. configs are
// paths under the config directory.
func SetupPaths(mods []SetupMod, configs []string) (SetupFiles, error) {
	paths := SetupFiles{}
	for _, mod := range mods {
		entries, err := setupManifest(&mod)
		if err != nil {
			return nil, err
		}
		for _, e := range setupPayloadEntries(entries) {
			var root SetupRoot
			if e.Parked {
				root = SetupRoot{parked: mod.FullName}
			}
			paths.add(root, e.Path)
		}
	}
	for _, name := range configs {
		if err := instance.CheckConfigPath(name); err != nil {
			return nil, fmt.Errorf("invalid saved config: %w", err)
		}
		if name == command.ConfigFile {
			return nil, fmt.Errorf("invalid saved config name %q", name)
		}
		paths.add(SetupRoot{}, instance.ConfigDir+"/"+name)
	}
	return paths, nil
}

func unionSetupPaths(a, b SetupFiles) SetupJournal {
	all := SetupFiles{}
	for _, set := range []SetupFiles{a, b} {
		for root, paths := range set {
			for p := range paths {
				all.add(root, p)
			}
		}
	}
	journal := SetupJournal{}
	for root, paths := range all {
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		journal.Roots = append(journal.Roots, SetupRootPaths{Root: root.String(), Paths: list})
	}
	sort.Slice(journal.Roots, func(i, j int) bool { return journal.Roots[i].Root < journal.Roots[j].Root })
	return journal
}

// PreflightSetupTargets refuses a target path that a file no package manages already occupies.
func PreflightSetupTargets(inst *store.Instance, current, target SetupFiles) error {
	for rootName, paths := range target {
		root, err := os.OpenRoot(rootName.dir(inst))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open %s for restore preflight: %w", rootName, err)
		}
		for path := range paths {
			if current[rootName][path] {
				continue
			}
			_, err := root.Stat(filepath.FromSlash(path))
			if err == nil {
				_ = root.Close()
				return unmanagedTarget(path)
			}
			if !errors.Is(err, os.ErrNotExist) {
				_ = root.Close()
				return fmt.Errorf("inspect %s in %s: %w", path, rootName, err)
			}
		}
		_ = root.Close()
	}
	return nil
}

// unmanagedTarget is the preflight error for a target path a file outside the current setup
// occupies. A config file is outside it when the config editor does not list it, such as a
// YAML, JSON or text file over 1 MiB.
func unmanagedTarget(path string) error {
	if installer.UserConfig(path) {
		return fmt.Errorf("unmanaged file already occupies %s: the current setup leaves it out, "+
			"as it does a YAML, JSON or text file over 1 MiB; move or shrink it first", path)
	}
	return fmt.Errorf("unmanaged file already occupies %s", path)
}

// stageSetupConfigs writes the config files of snap into staging's target tree: whole from the
// snapshot for a setup saved before it held the config tree, from verified blobs otherwise.
func stageSetupConfigs(dataRoot, staging string, snap *SetupSnapshot) error {
	dir := filepath.Join(staging, "target", "server", filepath.FromSlash(instance.ConfigDir))
	write := func(name string, content []byte) error {
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("create config staging directory: %w", err)
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return fmt.Errorf("stage config %s: %w", name, err)
		}
		return nil
	}
	for _, cfg := range snap.Configs {
		if len(cfg.Content) > instance.MaxConfigSize {
			return fmt.Errorf("saved config %s exceeds size limit", cfg.File)
		}
		if err := write(cfg.File, []byte(cfg.Content)); err != nil {
			return err
		}
	}
	blobs := setupblob.New(dataRoot)
	for _, c := range snap.ConfigFiles {
		content, err := ReadSetupConfig(blobs, c)
		if err != nil {
			return err
		}
		if err := write(c.File, content); err != nil {
			return err
		}
	}
	return nil
}

// ReadSetupConfig returns the saved bytes of one config file of a setup, verified against its
// digest.
func ReadSetupConfig(blobs *setupblob.Store, c store.SetupConfigRef) ([]byte, error) {
	path, err := blobs.Verify(c.SHA256)
	if err != nil {
		return nil, fmt.Errorf("verify saved config %s: %w", c.File, err)
	}
	content, err := os.ReadFile(path) // #nosec G304 -- Verify confines path to the blob directory.
	if err != nil {
		return nil, fmt.Errorf("read saved config %s: %w", c.File, err)
	}
	return content, nil
}

// ApplySetupPaths removes the current managed files and writes the target's from staging.
func ApplySetupPaths(inst *store.Instance, staging string, current, target SetupFiles) error {
	for rootName, paths := range current {
		root := rootName.dir(inst)
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		if err := installer.Remove(list, root); err != nil {
			return fmt.Errorf("remove current %s files: %w", rootName, err)
		}
	}
	for rootName, paths := range target {
		if err := applySetupRoot(inst, staging, rootName, paths, current[rootName]); err != nil {
			return err
		}
	}
	return nil
}

// applySetupRoot writes one root's saved files from staging over what removal left.
func applySetupRoot(inst *store.Instance, staging string, rootName SetupRoot, paths, replaced map[string]bool) error {
	root, sourceRoot := rootName.dir(inst), rootName.staged(staging)
	changes := make([]installer.Change, 0, len(paths))
	for p := range paths {
		changes = append(changes, installer.Change{
			Placement: installer.Placement{
				Source: filepath.Join(sourceRoot, filepath.FromSlash(p)), Dest: p,
			},
			Action: installer.ActionCreate,
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Dest < changes[j].Dest })
	if err := installer.Apply(changes, root); err != nil {
		return fmt.Errorf("apply saved %s files: %w", rootName, err)
	}
	return dropPendingConfigs(root, replaced, paths)
}

// dropPendingConfigs removes the pending copy of every config a restore replaced or removed, in
// the config tree or as a `.cfg` elsewhere, so the next settle can't put an older edit back over
// the restored file.
func dropPendingConfigs(rootPath string, sets ...map[string]bool) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", rootPath, err)
	}
	defer func() { _ = root.Close() }()
	for _, set := range sets {
		for p := range set {
			if !strings.HasSuffix(p, ".cfg") && !installer.UserConfig(p) {
				continue
			}
			if err := modconfig.DropPending(root, filepath.FromSlash(p)); err != nil {
				return err
			}
		}
	}
	return nil
}

func setupStateRows(
	instanceID string,
	snap *SetupSnapshot,
) ([]store.InstanceMod, store.InstanceLaunch, store.BackupPolicy, error) {
	mods := make([]store.InstanceMod, 0, len(snap.Mods))
	for _, mod := range snap.Mods {
		src, ok := source.ByName(mod.Source)
		if !ok {
			return nil, store.InstanceLaunch{}, store.BackupPolicy{}, fmt.Errorf("unknown mod source %s", mod.Source)
		}
		if _, err := setupManifest(&mod); err != nil {
			return nil, store.InstanceLaunch{}, store.BackupPolicy{}, err
		}
		mods = append(mods, store.InstanceMod{
			InstanceID: instanceID, FullName: mod.FullName, Source: src,
			Version: mod.Version, InstalledAs: mod.InstalledAs, Side: mod.Side,
			Enabled: mod.Enabled, Locked: mod.Locked, FileManifest: mod.FileManifest,
			InstalledAt: store.Now(),
		})
	}
	var preset, extra, modifiers *string
	if snap.Instance.Preset != "" {
		preset = &snap.Instance.Preset
	}
	if snap.Instance.ExtraArgs != "" {
		extra = &snap.Instance.ExtraArgs
	}
	if len(snap.Instance.Modifiers) > 0 {
		raw, err := json.Marshal(snap.Instance.Modifiers)
		if err != nil {
			return nil, store.InstanceLaunch{}, store.BackupPolicy{}, fmt.Errorf("encode saved modifiers: %w", err)
		}
		value := string(raw)
		modifiers = &value
	}
	launch := store.InstanceLaunch{
		ServerName: snap.Instance.ServerName, Public: snap.Instance.Public,
		Crossplay: snap.Instance.Crossplay, Preset: preset, Modifiers: modifiers,
		ExtraArgs: extra, MemLimitMB: snap.Instance.MemLimitMB, CPULimit: snap.Instance.CPULimit,
	}
	backup := store.BackupPolicy{
		KeepCold: snap.Instance.BackupKeepCold, KeepHot: snap.Instance.BackupKeepHot,
		OnRestart: snap.Instance.BackupOnRestart,
	}
	return mods, launch, backup, nil
}

type setupRestorePlan struct {
	Instance *store.Instance
	Snapshot SetupSnapshot
	// Current is the server's setup before the restore, for the automatic setup.
	Current      SetupSnapshot
	CurrentPaths SetupFiles
	TargetPaths  SetupFiles
	Journal      SetupJournal
	ModsChanged  bool
}

func (s *SetupJobs) prepareSetupRestore(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, row *store.SavedSetup,
	refs []store.SetupArtifactRef, payload *SetupJobPayload,
) (setupRestorePlan, error) {
	fresh, err := s.stoppedInstance(ctx, inst.ID)
	if err != nil {
		return setupRestorePlan{}, err
	}
	inst = fresh
	current, etag, err := (&SetupState{DB: s.DB}).Current(ctx, inst)
	if err != nil {
		return setupRestorePlan{}, err
	}
	if etag != payload.ETag {
		return setupRestorePlan{}, errors.New("server state changed after the restore preview")
	}
	snap, err := DecodeSetupSnapshot(row)
	if err != nil {
		return setupRestorePlan{}, err
	}
	if err := s.ValidateSettings(ctx, inst, &snap.Instance); err != nil {
		return setupRestorePlan{}, err
	}
	scoped := current.ScopedTo(&snap)
	currentPaths, err := SetupPaths(current.Mods, scoped.ConfigPaths())
	if err != nil {
		return setupRestorePlan{}, err
	}
	targetPaths, err := SetupPaths(snap.Mods, snap.ConfigPaths())
	if err != nil {
		return setupRestorePlan{}, err
	}
	if snap.Light {
		jh.Progress(ctx, 20, "verifying saved package files and fetching the rest from their registries")
	} else {
		jh.Progress(ctx, 20, "verifying saved package files")
	}
	artifacts := &setupArtifacts{DataRoot: s.DataRoot, Archives: s.Archives}
	if err := artifacts.stage(ctx, &snap, refs, payload.StagingDir); err != nil {
		return setupRestorePlan{}, err
	}
	if err := stageSetupConfigs(s.DataRoot, payload.StagingDir, &snap); err != nil {
		return setupRestorePlan{}, err
	}
	if err := PreflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
		return setupRestorePlan{}, err
	}
	fresh, err = s.stoppedInstance(ctx, inst.ID)
	if err != nil {
		return setupRestorePlan{}, err
	}
	_, after, err := (&SetupState{DB: s.DB}).Current(ctx, fresh)
	if err != nil {
		return setupRestorePlan{}, err
	}
	if after != etag {
		return setupRestorePlan{}, errors.New("server state changed after the restore preview")
	}
	return setupRestorePlan{
		Instance: fresh, Snapshot: snap, Current: current, CurrentPaths: currentPaths,
		TargetPaths: targetPaths, Journal: unionSetupPaths(currentPaths, targetPaths),
		ModsChanged: setupModsChanged(current.Mods, snap.Mods),
	}, nil
}

func (s *SetupJobs) applyStagedSetup(
	ctx context.Context, jh *jobs.Handle, plan *setupRestorePlan, payload *SetupJobPayload,
) error {
	if err := AssertStopped(ctx, s.Runtime, plan.Instance); err != nil {
		return err
	}
	jh.Progress(ctx, 80, "restoring managed files and configuration")
	apply := s.Apply
	if apply == nil {
		apply = ApplySetupPaths
	}
	if err := apply(plan.Instance, payload.StagingDir, plan.CurrentPaths, plan.TargetPaths); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore cancelled: %w", err)
	}
	if err := AssertStopped(ctx, s.Runtime, plan.Instance); err != nil {
		return err
	}
	if err := jh.Checkpoint(ctx, "applied"); err != nil {
		return fmt.Errorf("checkpoint applied setup: %w", err)
	}
	return nil
}

// RunRestore applies a saved setup to a stopped instance for requestedBy: it snapshots the world
// when mods change, saves the current setup as an automatic one linked to that snapshot and the
// current files for rollback, applies the staged files, and commits the rows in the job's finish.
// A failed apply rolls the files back.
//
//nolint:gocognit // The restore phases share a single rollback boundary.
func (s *SetupJobs) RunRestore(
	inst *store.Instance, row *store.SavedSetup, refs []store.SetupArtifactRef, payload *SetupJobPayload,
	requestedBy string,
) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		keepStaging := false
		cleanup := func(context.Context) {
			if !keepStaging {
				_ = os.RemoveAll(payload.StagingDir)
			}
		}
		fail := func(err error, archived func(context.Context, *sql.Tx) error) jobs.Outcome {
			out := jobs.Failed(errcode.Internal, err)
			out.AfterFinish = cleanup
			return out.PrependFinish(archived)
		}
		plan, err := s.prepareSetupRestore(ctx, jh, inst, row, refs, payload)
		if err != nil {
			return fail(err, nil)
		}
		inst = plan.Instance
		if err := ctx.Err(); err != nil {
			return fail(err, nil)
		}
		archived, saveBefore, err := s.saveBeforeRestore(ctx, jh, &plan, row, payload.StagingDir, requestedBy)
		if err != nil {
			return fail(err, nil)
		}
		jh.Progress(ctx, 65, "saving current files for rollback")
		if err := BackupSetupPaths(inst, payload.StagingDir, plan.Journal); err != nil {
			return fail(err, archived)
		}
		if err := WriteSetupJournal(payload.StagingDir, plan.Journal); err != nil {
			return fail(err, archived)
		}
		if err := jh.Checkpoint(ctx, "backed_up"); err != nil {
			return fail(err, archived)
		}
		written, err := s.trackReplacedConfigs(ctx, inst, payload.StagingDir, plan.Journal, plan.Snapshot.ConfigPaths())
		if err != nil {
			return fail(err, archived)
		}
		rollback := func(cause error) jobs.Outcome {
			if err := rollbackSetupPaths(inst, payload.StagingDir, plan.Journal); err != nil {
				keepStaging = true
				cause = errors.Join(
					cause,
					fmt.Errorf("rollback failed; recovery files remain in %s: %w", payload.StagingDir, err),
				)
				out := jobs.Failed(errcode.Internal, cause)
				out.OnFinish = finishToError(inst.ID, instance.StateStopped)
				return out.PrependFinish(archived)
			}
			return fail(cause, archived)
		}
		if err := s.applyStagedSetup(ctx, jh, &plan, payload); err != nil {
			return rollback(err)
		}
		mods, launch, backup, err := setupStateRows(inst.ID, &plan.Snapshot)
		if err != nil {
			return rollback(err)
		}
		jh.Progress(ctx, 100, "setup restored; server remains stopped")
		out := jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: joinFinish(func(ctx context.Context, tx *sql.Tx) error {
				if err := store.TxApplySetupState(ctx, tx, inst.ID, mods, &launch, backup); err != nil {
					return err
				}
				return store.TxRecordConfigRevisions(ctx, tx, written)
			}, saveBefore),
			AfterFinish: cleanup,
		}
		return out.PrependFinish(archived)
	}
}

// saveBeforeRestore takes what a restore of row saves before it changes anything: a pre-update
// snapshot of the world when mods change, and the current setup as an automatic one linked to
// it, recorded as caused by requestedBy. It returns the finish steps that record each, nil for
// what was not taken; a setup that cannot be captured is logged on jh, and only a failed
// snapshot is an error.
func (s *SetupJobs) saveBeforeRestore(
	ctx context.Context, jh *jobs.Handle, plan *setupRestorePlan, row *store.SavedSetup,
	staging, requestedBy string,
) (archived, saveBefore func(context.Context, *sql.Tx) error, err error) {
	before := automaticSetup{
		name: "Before restoring " + row.Name, createdBy: requestedBy, spare: row.ID,
		staging: filepath.Join(staging, "before"), before: "restore",
	}
	if plan.ModsChanged {
		jh.Progress(ctx, 55, "backing up the world")
		world, err := s.Worlds.snapshotRow(ctx, plan.Instance, store.TriggerPreUpdate)
		if err != nil {
			return nil, nil, err
		}
		if world != nil {
			archived, before.worldID = recordSnapshot(world), world.ID
		}
	}
	jh.Progress(ctx, 60, "saving the current setup")
	return archived, s.saveAutomatic(ctx, jh, plan.Instance, &plan.Current, &before), nil
}

// trackReplacedConfigs records, in each file's history, the config files a restore is about to
// replace with other bytes or remove, reading the copies the rollback backup just saved, and
// returns the revisions of the files it writes, leaving out any too large to keep. A target file
// that does not exist yet is
// recorded empty, so its restored settings count as tweaks. targets are paths under the config
// directory, already staged.
func (s *SetupJobs) trackReplacedConfigs(
	ctx context.Context, inst *store.Instance, staging string, journal SetupJournal, targets []string,
) ([]store.ConfigRevision, error) {
	saved, err := savedSetupConfigs(staging, journal)
	if err != nil {
		return nil, err
	}
	stagedDir := filepath.Join(staging, "target", "server", filepath.FromSlash(instance.ConfigDir))
	var written []store.ConfigRevision
	for _, name := range targets {
		next, err := os.ReadFile(filepath.Join(stagedDir, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("read the staged %s: %w", name, err)
		}
		current, ok := saved[name]
		if ok && bytes.Equal(current, next) {
			delete(saved, name)
			continue
		}
		if !ok {
			saved[name] = []byte{}
		}
		if len(next) <= store.MaxConfigRevisionBytes {
			written = append(written, store.ConfigRevision{
				InstanceID: inst.ID, File: name, Source: store.ConfigSetup, Content: next,
			})
		}
	}
	if len(saved) == 0 {
		return written, nil
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		dir = nil
	case err != nil:
		return nil, fmt.Errorf("open config directory: %w", err)
	default:
		defer func() { _ = dir.Close() }()
	}
	for name, raw := range saved {
		if err := trackBefore(ctx, s.DB, dir, inst.ID, name, raw); err != nil {
			return nil, err
		}
	}
	return written, nil
}

// savedSetupConfigs reads the config files the rollback backup saved from the server root, by
// name relative to the config directory.
func savedSetupConfigs(staging string, journal SetupJournal) (map[string][]byte, error) {
	saved := map[string][]byte{}
	for i, group := range journal.Roots {
		if group.Root != (SetupRoot{}).String() {
			continue
		}
		backup := filepath.Join(staging, "backup", fmt.Sprintf("%d", i))
		for _, p := range group.Paths {
			name, ok := strings.CutPrefix(p, instance.ConfigDir+"/")
			if !ok {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(backup, filepath.FromSlash(p)))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read the saved %s: %w", name, err)
			}
			saved[name] = raw
		}
	}
	return saved, nil
}
