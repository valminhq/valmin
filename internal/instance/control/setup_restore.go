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
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

func SetupPaths(mods []SetupMod, configs []ManifestConfig) (map[string]map[string]bool, error) {
	paths := map[string]map[string]bool{}
	add := func(root, path string) {
		if paths[root] == nil {
			paths[root] = map[string]bool{}
		}
		paths[root][path] = true
	}
	for _, mod := range mods {
		entries, err := SetupManifest(mod)
		if err != nil {
			return nil, err
		}
		for _, e := range SetupPayloadEntries(entries) {
			root := "server"
			if e.Parked {
				root = "park:" + mod.FullName
			}
			add(root, e.Path)
		}
	}
	for _, cfg := range configs {
		if cfg.File == "" || cfg.File != filepath.Base(cfg.File) ||
			!strings.HasSuffix(cfg.File, ".cfg") || strings.ContainsAny(cfg.File, `/\`) ||
			cfg.File == command.ConfigFile {
			return nil, fmt.Errorf("invalid saved config name %q", cfg.File)
		}
		if len(cfg.Content) > maxSetupConfigSize {
			return nil, fmt.Errorf("saved config %s exceeds size limit", cfg.File)
		}
		add("server", setupConfigDir+"/"+cfg.File)
	}
	return paths, nil
}

func unionSetupPaths(a, b map[string]map[string]bool) SetupJournal {
	all := map[string]map[string]bool{}
	for _, set := range []map[string]map[string]bool{a, b} {
		for root, paths := range set {
			if all[root] == nil {
				all[root] = map[string]bool{}
			}
			for p := range paths {
				all[root][p] = true
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
		journal.Roots = append(journal.Roots, SetupRootPaths{Root: root, Paths: list})
	}
	sort.Slice(journal.Roots, func(i, j int) bool { return journal.Roots[i].Root < journal.Roots[j].Root })
	return journal
}

func PreflightSetupTargets(
	inst *store.Instance, current, target map[string]map[string]bool,
) error {
	for rootName, paths := range target {
		rootPath, err := SetupRoot(inst, rootName)
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(rootPath)
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
		dest := filepath.Join(staging, "target", "server", filepath.FromSlash(setupConfigDir), cfg.File)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("create config staging directory: %w", err)
		}
		if err := os.WriteFile(dest, []byte(cfg.Content), 0o600); err != nil {
			return fmt.Errorf("stage config %s: %w", cfg.File, err)
		}
	}
	return nil
}

func ApplySetupPaths(inst *store.Instance, staging string, current, target map[string]map[string]bool) error {
	for rootName, paths := range current {
		root, err := SetupRoot(inst, rootName)
		if err != nil {
			return err
		}
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
		root, err := SetupRoot(inst, rootName)
		if err != nil {
			return err
		}
		sourceRoot := filepath.Join(staging, "target", "server")
		if mod, ok := strings.CutPrefix(rootName, "park:"); ok {
			sourceRoot = filepath.Join(staging, "target", "park", mod)
		}
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
		if _, err := SetupManifest(mod); err != nil {
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
	CurrentPaths map[string]map[string]bool
	TargetPaths  map[string]map[string]bool
	Journal      SetupJournal
	ModsChanged  bool
}

func (s *SetupJobs) prepareSetupRestore(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, row *store.SavedSetup,
	refs []store.SetupArtifactRef, payload *SetupJobPayload,
) (setupRestorePlan, error) {
	fresh, err := s.StoppedInstance(ctx, inst.ID)
	if err != nil {
		return setupRestorePlan{}, err
	}
	inst = fresh
	_, etag, err := (&SetupState{DB: s.DB}).Current(ctx, inst)
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
	current, err := (&SetupState{DB: s.DB}).Capture(ctx, inst)
	if err != nil {
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
	if err := (&SetupArtifacts{DataRoot: s.DataRoot}).Stage(ctx, &snap, refs, payload.StagingDir); err != nil {
		return setupRestorePlan{}, err
	}
	if err := writeSetupTargetConfigs(payload.StagingDir, snap.Configs); err != nil {
		return setupRestorePlan{}, err
	}
	if err := PreflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
		return setupRestorePlan{}, err
	}
	fresh, err = s.StoppedInstance(ctx, inst.ID)
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
		ModsChanged: SetupModsChanged(current.Mods, snap.Mods),
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
			out := SetupFailed(err)
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
			if err := RollbackSetupPaths(inst, payload.StagingDir, plan.Journal); err != nil {
				keepStaging = true
				cause = errors.Join(
					cause,
					fmt.Errorf("rollback failed; recovery files remain in %s: %w", payload.StagingDir, err),
				)
				out := SetupFailed(cause)
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
