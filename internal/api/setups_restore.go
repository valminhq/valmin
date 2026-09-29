package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

type setupRootPaths struct {
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
}

type setupJournal struct {
	Roots []setupRootPaths `json:"roots"`
}

func setupRoot(inst *store.Instance, name string) (string, error) {
	if name == "server" {
		return serverDir(inst), nil
	}
	mod, ok := strings.CutPrefix(name, "park:")
	if !ok {
		return "", fmt.Errorf("invalid setup root %q", name)
	}
	if err := installer.CheckFullName(mod); err != nil {
		return "", fmt.Errorf("validate parked package %s: %w", mod, err)
	}
	return parkedPackageDir(inst, mod), nil
}

func setupPaths(mods []setupMod, configs []manifestConfig) (map[string]map[string]bool, error) {
	paths := map[string]map[string]bool{}
	add := func(root, path string) {
		if paths[root] == nil {
			paths[root] = map[string]bool{}
		}
		paths[root][path] = true
	}
	for _, mod := range mods {
		entries, err := setupManifest(mod)
		if err != nil {
			return nil, err
		}
		for _, e := range setupPayloadEntries(entries) {
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
		if len(cfg.Content) > maxManifestConfigSize {
			return nil, fmt.Errorf("saved config %s exceeds size limit", cfg.File)
		}
		add("server", configDir+"/"+cfg.File)
	}
	return paths, nil
}

func unionSetupPaths(a, b map[string]map[string]bool) setupJournal {
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
	journal := setupJournal{}
	for root, paths := range all {
		list := make([]string, 0, len(paths))
		for p := range paths {
			list = append(list, p)
		}
		sort.Strings(list)
		journal.Roots = append(journal.Roots, setupRootPaths{Root: root, Paths: list})
	}
	sort.Slice(journal.Roots, func(i, j int) bool { return journal.Roots[i].Root < journal.Roots[j].Root })
	return journal
}

func writeSetupJournal(staging string, journal setupJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode setup rollback journal: %w", err)
	}
	journalPath := filepath.Join(staging, "journal.json")
	f, err := os.OpenFile(journalPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // panel-generated path
	if err != nil {
		return fmt.Errorf("create setup rollback journal: %w", err)
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("write setup rollback journal: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close setup rollback journal: %w", closeErr)
	}
	stageRoot, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("open setup staging directory: %w", err)
	}
	defer func() { _ = stageRoot.Close() }()
	dir, err := stageRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open setup staging directory for sync: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync setup staging directory: %w", err)
	}
	return nil
}

func readSetupJournal(staging string) (setupJournal, error) {
	root, err := os.OpenRoot(staging)
	if err != nil {
		return setupJournal{}, fmt.Errorf("open setup staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open("journal.json")
	if err != nil {
		return setupJournal{}, fmt.Errorf("open setup rollback journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	if err != nil {
		return setupJournal{}, fmt.Errorf("read setup rollback journal: %w", err)
	}
	var journal setupJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return setupJournal{}, fmt.Errorf("decode setup rollback journal: %w", err)
	}
	return journal, nil
}

func backupSetupPaths(inst *store.Instance, staging string, journal setupJournal) error {
	for i, group := range journal.Roots {
		root, err := setupRoot(inst, group.Root)
		if err != nil {
			return err
		}
		if err := installer.BackupPaths(group.Paths, root,
			filepath.Join(staging, "backup", fmt.Sprintf("%d", i))); err != nil {
			return fmt.Errorf("back up %s files for setup restore: %w", group.Root, err)
		}
	}
	return nil
}

func rollbackSetupPaths(inst *store.Instance, staging string, journal setupJournal) error {
	var errs []error
	for i, group := range journal.Roots {
		root, err := setupRoot(inst, group.Root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		entries := make([]installer.ManifestEntry, 0, len(group.Paths))
		for _, p := range group.Paths {
			entries = append(entries, installer.ManifestEntry{Path: p})
		}
		if err := installer.Rollback(entries, root,
			filepath.Join(staging, "backup", fmt.Sprintf("%d", i))); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", group.Root, err))
		}
	}
	return errors.Join(errs...)
}

func preflightSetupTargets(
	inst *store.Instance, current, target map[string]map[string]bool,
) error {
	for rootName, paths := range target {
		rootPath, err := setupRoot(inst, rootName)
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

func writeSetupTargetConfigs(staging string, configs []manifestConfig) error {
	for _, cfg := range configs {
		dest := filepath.Join(staging, "target", "server", filepath.FromSlash(configDir), cfg.File)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("create config staging directory: %w", err)
		}
		if err := os.WriteFile(dest, []byte(cfg.Content), 0o600); err != nil {
			return fmt.Errorf("stage config %s: %w", cfg.File, err)
		}
	}
	return nil
}

func applySetupPaths(inst *store.Instance, staging string, current, target map[string]map[string]bool) error {
	for rootName, paths := range current {
		root, err := setupRoot(inst, rootName)
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
		root, err := setupRoot(inst, rootName)
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
	snap *setupSnapshot,
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
	Snapshot     setupSnapshot
	CurrentPaths map[string]map[string]bool
	TargetPaths  map[string]map[string]bool
	Journal      setupJournal
	ModsChanged  bool
}

func (h *Instances) prepareSetupRestore(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, row *store.SavedSetup,
	refs []store.SetupArtifactRef, payload *setupJobPayload,
) (setupRestorePlan, error) {
	fresh, err := h.stoppedSetupInstance(ctx, inst.ID)
	if err != nil {
		return setupRestorePlan{}, err
	}
	inst = fresh
	_, etag, err := h.currentSetupState(ctx, inst)
	if err != nil {
		return setupRestorePlan{}, err
	}
	if etag != payload.ETag {
		return setupRestorePlan{}, errors.New("server state changed after the restore preview")
	}
	snap, err := decodeSetupSnapshot(row)
	if err != nil {
		return setupRestorePlan{}, err
	}
	if err := h.validateSetupSettings(ctx, inst, &snap.Instance); err != nil {
		return setupRestorePlan{}, err
	}
	current, err := h.captureSetupSnapshot(ctx, inst)
	if err != nil {
		return setupRestorePlan{}, err
	}
	currentPaths, err := setupPaths(current.Mods, current.Configs)
	if err != nil {
		return setupRestorePlan{}, err
	}
	targetPaths, err := setupPaths(snap.Mods, snap.Configs)
	if err != nil {
		return setupRestorePlan{}, err
	}
	jh.Progress(ctx, 20, "verifying saved package files")
	if err := h.stageSetupArtifacts(ctx, &snap, refs, payload.StagingDir); err != nil {
		return setupRestorePlan{}, err
	}
	if err := writeSetupTargetConfigs(payload.StagingDir, snap.Configs); err != nil {
		return setupRestorePlan{}, err
	}
	if err := preflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
		return setupRestorePlan{}, err
	}
	fresh, err = h.stoppedSetupInstance(ctx, inst.ID)
	if err != nil {
		return setupRestorePlan{}, err
	}
	_, after, err := h.currentSetupState(ctx, fresh)
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

func (h *Instances) applyStagedSetup(
	ctx context.Context, jh *jobs.Handle, plan *setupRestorePlan, payload *setupJobPayload,
) error {
	if err := h.assertStopped(ctx, plan.Instance); err != nil {
		return err
	}
	jh.Progress(ctx, 80, "restoring managed files and configuration")
	apply := h.setupApply
	if apply == nil {
		apply = applySetupPaths
	}
	if err := apply(plan.Instance, payload.StagingDir, plan.CurrentPaths, plan.TargetPaths); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore cancelled: %w", err)
	}
	if err := h.assertStopped(ctx, plan.Instance); err != nil {
		return err
	}
	if err := jh.Checkpoint(ctx, "applied"); err != nil {
		return fmt.Errorf("checkpoint applied setup: %w", err)
	}
	return nil
}

//nolint:gocognit // The restore phases share a single rollback boundary.
func (h *Instances) runSetupRestore(
	inst *store.Instance, row *store.SavedSetup, refs []store.SetupArtifactRef, payload *setupJobPayload,
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
			return withArchive(out, archived)
		}
		plan, err := h.prepareSetupRestore(ctx, jh, inst, row, refs, payload)
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
			archived, err = h.snapshotWorlds(ctx, inst, store.TriggerPreUpdate)
			if err != nil {
				return fail(err, nil)
			}
		}
		jh.Progress(ctx, 65, "saving current files for rollback")
		if err := backupSetupPaths(inst, payload.StagingDir, plan.Journal); err != nil {
			return fail(err, archived)
		}
		if err := writeSetupJournal(payload.StagingDir, plan.Journal); err != nil {
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
				return withArchive(out, archived)
			}
			return fail(cause, archived)
		}
		if err := h.applyStagedSetup(ctx, jh, &plan, payload); err != nil {
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
		return withArchive(out, archived)
	}
}

func sweepSetupRestore(ctx context.Context, h *Instances, job *store.Job) {
	var payload setupJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil || job.InstanceID == nil {
		slog.ErrorContext(ctx, "interrupted setup restore has invalid payload", slog.String("job_id", job.ID))
		return
	}
	if payload.StagingDir == "" || !withinRoot(setupStagingRoot(h.Cfg.Data.Root), payload.StagingDir) {
		slog.ErrorContext(ctx, "interrupted setup restore has unsafe staging path", slog.String("job_id", job.ID))
		return
	}
	journal, err := readSetupJournal(payload.StagingDir)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(payload.StagingDir)
		return
	}
	if err != nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore journal unreadable",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return
	}
	inst, err := h.DB.InstanceByID(ctx, *job.InstanceID)
	if err != nil || inst == nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore instance unavailable",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return
	}
	if err := rollbackSetupPaths(inst, payload.StagingDir, journal); err != nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore rollback failed",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		_, _ = instance.SetState(ctx, h.DB, inst.ID, instance.StateStopped, instance.StateError)
		return
	}
	_ = os.RemoveAll(payload.StagingDir)
}

func sweepSetupSave(ctx context.Context, h *Instances, job *store.Job) {
	var payload setupJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return
	}
	if payload.StagingDir != "" && withinRoot(setupStagingRoot(h.Cfg.Data.Root), payload.StagingDir) {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "remove interrupted setup save staging", slog.Any("error", err))
		}
	}
}
