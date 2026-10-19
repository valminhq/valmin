package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// SetupPaths is every file a setup of mods and configs manages, by root.
func SetupPaths(mods []SetupMod, configs []ManifestConfig) (SetupFiles, error) {
	paths := SetupFiles{}
	for _, mod := range mods {
		entries, err := setupManifest(mod)
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
	for _, cfg := range configs {
		if err := CheckManifestConfigName(cfg.File); err != nil {
			return nil, fmt.Errorf("invalid saved config: %w", err)
		}
		if cfg.File == command.ConfigFile {
			return nil, fmt.Errorf("invalid saved config name %q", cfg.File)
		}
		if len(cfg.Content) > MaxConfigSize {
			return nil, fmt.Errorf("saved config %s exceeds size limit", cfg.File)
		}
		paths.add(SetupRoot{}, instance.ConfigDir+"/"+cfg.File)
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
				return fmt.Errorf("unmanaged file already occupies %s", path)
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

func writeSetupTargetConfigs(staging string, configs []ManifestConfig) error {
	for _, cfg := range configs {
		dest := filepath.Join(staging, "target", "server", filepath.FromSlash(instance.ConfigDir), cfg.File)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("create config staging directory: %w", err)
		}
		if err := os.WriteFile(dest, []byte(cfg.Content), 0o600); err != nil {
			return fmt.Errorf("stage config %s: %w", cfg.File, err)
		}
	}
	return nil
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

// dropPendingConfigs removes the pending copy of every config a restore replaced or removed,
// so the next settle can't put an older edit back over the restored file.
func dropPendingConfigs(rootPath string, sets ...map[string]bool) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", rootPath, err)
	}
	defer func() { _ = root.Close() }()
	for _, set := range sets {
		for p := range set {
			if !strings.HasSuffix(p, ".cfg") {
				continue
			}
			if err := modconfig.DropPending(root, filepath.FromSlash(p)); err != nil {
				return err //nolint:wrapcheck // DropPending names the file
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
		if _, err := setupManifest(mod); err != nil {
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
	Instance     *store.Instance
	Snapshot     SetupSnapshot
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
	currentPaths, err := SetupPaths(current.Mods, current.Configs)
	if err != nil {
		return setupRestorePlan{}, err
	}
	targetPaths, err := SetupPaths(snap.Mods, snap.Configs)
	if err != nil {
		return setupRestorePlan{}, err
	}
	jh.Progress(ctx, 20, "verifying saved package files")
	if err := (&setupArtifacts{DataRoot: s.DataRoot}).stage(ctx, &snap, refs, payload.StagingDir); err != nil {
		return setupRestorePlan{}, err
	}
	if err := writeSetupTargetConfigs(payload.StagingDir, snap.Configs); err != nil {
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
		Instance: fresh, Snapshot: snap, CurrentPaths: currentPaths,
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

//nolint:gocognit // The restore phases share a single rollback boundary.
func (s *SetupJobs) RunRestore(
	inst *store.Instance, row *store.SavedSetup, refs []store.SetupArtifactRef, payload *SetupJobPayload,
) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		keepStaging := false
		cleanup := func(context.Context) {
			if !keepStaging {
				_ = os.RemoveAll(payload.StagingDir)
			}
		}
		fail := func(err error, archived func(context.Context, *sql.Tx) error) jobs.Outcome {
			out := setupFailed(err)
			out.AfterFinish = cleanup
			return withSetupArchive(out, archived)
		}
		plan, err := s.prepareSetupRestore(ctx, jh, inst, row, refs, payload)
		if err != nil {
			return fail(err, nil)
		}
		inst = plan.Instance
		if err := ctx.Err(); err != nil {
			return fail(err, nil)
		}
		var archived func(context.Context, *sql.Tx) error
		if plan.ModsChanged {
			jh.Progress(ctx, 55, "backing up the world")
			archived, err = s.Snapshotter.Snapshot(ctx, inst, store.TriggerPreUpdate)
			if err != nil {
				return fail(err, nil)
			}
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
		rollback := func(cause error) jobs.Outcome {
			if err := rollbackSetupPaths(inst, payload.StagingDir, plan.Journal); err != nil {
				keepStaging = true
				cause = errors.Join(
					cause,
					fmt.Errorf("rollback failed; recovery files remain in %s: %w", payload.StagingDir, err),
				)
				out := setupFailed(cause)
				out.OnFinish = finishToError(inst.ID, instance.StateStopped)
				return withSetupArchive(out, archived)
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
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxApplySetupState(ctx, tx, inst.ID, mods, launch, backup)
			},
			AfterFinish: cleanup,
		}
		return withSetupArchive(out, archived)
	}
}

func withSetupArchive(out jobs.Outcome, archived func(context.Context, *sql.Tx) error) jobs.Outcome {
	if archived == nil {
		return out
	}
	then := out.OnFinish
	out.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
		if err := archived(ctx, tx); err != nil {
			return err
		}
		if then == nil {
			return nil
		}
		return then(ctx, tx)
	}
	return out
}
