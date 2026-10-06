package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

const bepinexConfig = "BepInEx/config/BepInEx.cfg"

const (
	CheckpointResolved        = "resolved"
	CheckpointDownloaded      = "downloaded"
	CheckpointStaged          = "staged"
	CheckpointBackedUp        = "backed_up"
	CheckpointManifestWritten = "manifest_written"
	CheckpointApplied         = "applied"
)

// Installer owns mod install submissions, staging, placement, and rollback.
type Installer struct {
	DB            *store.DB
	Engine        *jobs.Engine
	Commands      *command.Manager
	Clients       map[source.Source]*thunderstore.Client
	Caches        map[source.Source]*cache.Cache
	DataRoot      string
	ArchiveWorlds ArchiveWorlds
}

func (i *Installer) enabledSources() []source.Source {
	out := make([]source.Source, 0, len(i.Clients))
	for _, src := range source.All() {
		if _, ok := i.Clients[src]; ok {
			out = append(out, src)
		}
	}
	return out
}

func (i *Installer) planner() *Planner { return &Planner{DB: i.DB, Enabled: i.enabledSources()} }
func (i *Installer) newIndex(ctx context.Context, id string, prefer source.Source) *Index {
	return NewIndex(ctx, i.DB, id, prefer, i.enabledSources())
}

func (i *Installer) failureCode(err error) errcode.Code {
	if errors.Is(err, instance.ErrServerRunning) {
		return errcode.InstanceMustBeStopped
	}
	return errcode.Internal
}

func stagedPackageDir(stagingDir, fullName string) string {
	return filepath.Join(stagingDir, "pkg", fullName)
}

// Submit stages a directory and submits one mod_install job for payload, filling in its
// StagingDir. Every install goes through here, the single-package one and "Update all" alike.
// audit is the operator's request entry, nil for a job a definition chain submits on its own.
func (i *Installer) Submit(
	ctx context.Context,
	inst *store.Instance,
	payload *InstallPayload,
	what string,
	requestedBy string,
	audit *store.AuditEntry,
	afterFinish func(context.Context),
) (*store.Job, error) {
	root := stagingRoot(i.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		return nil, fmt.Errorf("create the mod staging root: %w", err)
	}
	staging, err := os.MkdirTemp(root, what+"-*")
	if err != nil {
		return nil, fmt.Errorf("create a staging directory for a mod %s: %w", what, err)
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	id := inst.ID
	payload.StagingDir = staging
	job, err := i.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModInstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: *payload,
		Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			// A stopped→stopped compare-and-swap: the kind holds the lock without moving
			// the state, and the CAS makes "still stopped" atomic with taking the lock.
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim mod_install for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, i.runModInstallThen(inst, payload, afterFinish))
	if err != nil {
		// Not wrapped: writeJobSubmitError reads the typed conflicts out with errors.As.
		return nil, err //nolint:wrapcheck // the caller matches on the engine's typed errors
	}
	submitted = true
	return job, nil
}

// runModInstallThen is runModInstall with a continuation that runs only on success.
// Starting the server after a failed install would create the world unmodded, which is the
// outcome installing before first boot exists to prevent.
func (i *Installer) runModInstallThen(
	inst *store.Instance, payload *InstallPayload, afterFinish func(context.Context),
) jobs.Runner {
	run := i.runModInstall(inst, payload)
	if afterFinish == nil {
		return run
	}
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		out := run(ctx, h)
		if out.Status == jobs.StatusSucceeded {
			out.AfterFinish = afterFinish
		}
		return out
	}
}

// stagedPackage is one package of the closure, carried between the runner's phases.
type stagedPackage struct {
	fullName string
	// src is the registry this package's bytes come from. It is chosen once, during resolve,
	// and then drives the download, the cache root and the recorded install (B14).
	src        source.Source
	version    string
	transitive bool
	// remove is a package the change uninstalls: nothing is downloaded or placed, its row goes
	// in the job's Finish transaction, and its files come off as an update's stale files do.
	remove      bool
	zipPath     string
	stagingDir  string
	changes     []installer.Change
	manifest    []installer.ManifestEntry
	manifestRaw string
	// prev is the row this package replaces on an update, nil on a first install. Its file
	// manifest is the only exact record of what the old version put on disk: the placement
	// heuristics describe the package as published now, not as it was installed.
	prev *store.InstanceMod
	// prevManifest is prev's decoded manifest, and prevStale the part the new version does
	// not write — the files an update removes. The rest are overwritten in place by Apply.
	prevManifest []installer.ManifestEntry
	prevStale    []string
}

// writePrevRows records every row this install is about to replace, before it replaces it.
func writePrevRows(stagingDir string, pkgs []*stagedPackage) error {
	for _, p := range pkgs {
		if p.prev == nil {
			continue
		}
		if err := fsutil.MkdirAllExact(prevRowDir(stagingDir)); err != nil {
			return fmt.Errorf("create the staging directory for replaced rows: %w", err)
		}
		raw, err := json.Marshal(replaced{Row: *p.prev, Stale: p.prevStale})
		if err != nil {
			return fmt.Errorf("encode the replaced row for %s: %w", p.fullName, err)
		}
		if err := fsutil.WriteFileAtomic(prevRowPath(stagingDir, p.fullName), raw); err != nil {
			return fmt.Errorf("record the replaced row for %s: %w", p.fullName, err)
		}
	}
	return nil
}

// runModInstall is the mod_install Runner. The phase order is load-bearing: resolve,
// download, stage, write the manifest, then move files. Every failure at or after the
// manifest is undone from the manifest, the only exact record of what moved.
func (i *Installer) runModInstall(inst *store.Instance, payload *InstallPayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()

		if len(payload.Updates) > 0 {
			h.Log(updateSummary(payload.Updates))
		}
		pkgs, outcome := i.prepareInstall(ctx, h, inst, payload)
		if outcome != nil {
			return *outcome
		}
		if len(pkgs) == 0 {
			h.Progress(ctx, 100, "already installed; nothing to do")
			return jobs.Outcome{Status: jobs.StatusSucceeded}
		}
		// The archive comes after everything that can still be abandoned for free and before
		// the first file moves: a download that fails leaves no archive nobody needed, and the
		// world is saved before anything could change what it needs.
		var archived func(context.Context, *sql.Tx) error
		if payload.Backup || replacesInstalled(pkgs) {
			if h.CancelRequested(ctx) {
				return jobs.Outcome{Status: jobs.StatusCancelled}
			}
			var err error
			if archived, err = archiveBeforeUpdate(ctx, h, inst, i.ArchiveWorlds); err != nil {
				return modJobFailed(i.failureCode(err), err)
			}
		}
		return withArchive(i.commitInstall(ctx, h, inst, payload, pkgs), archived)
	}
}

// replacesInstalled reports whether any package moves an installed one to another version.
func replacesInstalled(pkgs []*stagedPackage) bool {
	return slices.ContainsFunc(pkgs, func(p *stagedPackage) bool { return p.prev != nil })
}

// prepareInstall is everything that can still be abandoned: resolve, download, unpack, and
// work out what would change. Nothing it does is visible in server/ or in the database, so
// a failure or a cancellation here needs no undoing beyond deleting the staging directory.
func (i *Installer) prepareInstall(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, payload *InstallPayload,
) ([]*stagedPackage, *jobs.Outcome) {
	h.Progress(ctx, 5, "resolving dependencies")
	pkgs, outcome := i.ResolveForInstall(ctx, inst, payload)
	if outcome != nil {
		return nil, outcome
	}
	if outcome := mark(ctx, h, CheckpointResolved); outcome != nil {
		return nil, outcome
	}
	if len(pkgs) == 0 {
		return nil, nil
	}

	h.Progress(ctx, 20, fmt.Sprintf("downloading %d packages", len(pkgs)))
	if err := i.downloadClosure(ctx, pkgs); err != nil {
		return nil, failed(modJobFailed(errcode.Unavailable, err))
	}
	if outcome := mark(ctx, h, CheckpointDownloaded); outcome != nil {
		return nil, outcome
	}

	h.Progress(ctx, 45, "unpacking")
	if err := stageClosure(pkgs, payload.StagingDir); err != nil {
		return nil, failed(modJobFailed(errcode.PackageInvalid, err))
	}
	if outcome := mark(ctx, h, CheckpointStaged); outcome != nil {
		return nil, outcome
	}

	h.Progress(ctx, 60, "checking what would change")
	if err := i.planClosure(ctx, inst.ID, serverDir(inst), pkgs); err != nil {
		return nil, failed(planFailure(err))
	}
	for _, p := range pkgs {
		h.Log(diffSummary(p))
	}
	if slices.ContainsFunc(pkgs, downgrades) {
		h.Log(
			"a downgrade does not restore the world or config files; the backup taken first holds the world as it was",
		)
	}
	return pkgs, nil
}

// downgrades reports whether a package moves an installed one to a lower version.
func downgrades(p *stagedPackage) bool {
	return !p.remove && p.prev != nil && newer(p.prev.Version, p.version)
}

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

	serverRoot := serverDir(inst)
	backupDir := stagingBackupDir(payload.StagingDir)

	h.Progress(ctx, 68, "saving what would be replaced")
	for _, p := range pkgs {
		if err := installer.Backup(p.changes, serverRoot, backupDir); err != nil {
			// Nothing is recorded and nothing has moved, so there is nothing to undo.
			return modJobFailed(errcode.Internal, fmt.Errorf("back up for %s: %w", p.fullName, err))
		}
		// An update's stale files are displaced too, so they are saved in the same pass and
		// before the same checkpoint: rollback's only rule is "restore what has a backup".
		if err := installer.BackupPaths(p.prevStale, serverRoot, backupDir); err != nil {
			return modJobFailed(errcode.Internal,
				fmt.Errorf("back up what %s replaces: %w", p.fullName, err))
		}
	}
	if err := writePrevRows(payload.StagingDir, pkgs); err != nil {
		return modJobFailed(errcode.Internal, err)
	}
	// The crash sweep undoes nothing before this checkpoint: no row has changed and no file
	// has moved.
	if err := h.Checkpoint(ctx, CheckpointBackedUp); err != nil {
		return modJobFailed(errcode.Internal, err)
	}

	h.Progress(ctx, 70, "recording the file manifest")
	if err := i.writeManifests(ctx, inst.ID, pkgs); err != nil {
		return modJobFailed(errcode.Internal, err)
	}

	// From here every failure goes through the rollback, including a checkpoint that will
	// not write: the rows and restart_required are committed, so a job ending terminal
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

// mark records a checkpoint, returning the terminal outcome if it could not be written — a
// job whose resume marker is not on the row is one the crash sweep would misread.
func mark(ctx context.Context, h *jobs.Handle, checkpoint string) *jobs.Outcome {
	if err := h.Checkpoint(ctx, checkpoint); err != nil {
		return failed(modJobFailed(errcode.Internal, err))
	}
	return nil
}

// ResolveForInstall computes the closure and drops the nodes that need no work. A nil
// outcome means the packages returned are the ones to install; a non-nil one is the
// terminal answer.
func (i *Installer) ResolveForInstall(
	ctx context.Context, inst *store.Instance, payload *InstallPayload,
) ([]*stagedPackage, *jobs.Outcome) {
	plan, idx, outcome := i.jobPlan(ctx, inst, payload)
	if outcome != nil {
		return nil, outcome
	}
	installed := idx.Rows()
	have := make(map[string]*store.InstanceMod, len(installed))
	for j := range installed {
		have[installed[j].FullName] = &installed[j]
	}

	out := make([]*stagedPackage, 0, len(plan.Removals)+len(plan.Closure.Nodes))
	for _, name := range plan.Removals {
		p, err := removedPackageOf(have[name])
		if err != nil {
			return nil, failed(modJobFailed(errcode.Internal, err))
		}
		out = append(out, p)
	}
	for _, n := range plan.Closure.Nodes {
		if n.NoOp {
			continue
		}
		p, outcome := stageNode(n, idx.SourceOf(n.FullName, n.Version), have[n.FullName], len(payload.Updates) > 0)
		if outcome != nil {
			return nil, outcome
		}
		out = append(out, p)
	}
	return out, nil
}

// jobPlan is the plan an install job carries out, or the terminal outcome refusing it before
// anything downloads: a conflict, or a disabled package in the closure, since an update to a
// parked package would place files beside the ones it parked and a mod depending on a disabled
// one would not load.
func (i *Installer) jobPlan(
	ctx context.Context, inst *store.Instance, payload *InstallPayload,
) (ChangePlan, *Index, *jobs.Outcome) {
	prefer, _ := source.ByName(payload.Source)
	idx := i.newIndex(ctx, inst.ID, prefer)
	plan, err := i.planPayload(ctx, inst, payload, idx)
	switch {
	case idx.err != nil:
		return plan, idx, failed(modJobFailed(errcode.Internal, idx.err))
	case err != nil:
		return plan, idx, failed(modJobFailed(resolveFailure(err), err))
	case len(plan.Conflicts) > 0:
		return plan, idx, failed(modJobFailed(errcode.ModConflict,
			fmt.Errorf("the change would break a dependency: %s", describeConflicts(plan.Conflicts))))
	}
	if off := DisabledInClosure(ClosureNames(plan.Closure), idx.Rows()); len(off) > 0 {
		return plan, idx, failed(modJobFailed(errcode.ModConflict,
			fmt.Errorf("these mods are disabled; enable them first: %s", strings.Join(off, ", "))))
	}
	return plan, idx, nil
}

// stageNode turns one resolved node into the package the job places. current is the installed
// row of that package, nil when it is not installed; update is true for "Update all".
func stageNode(
	n modresolver.Node, src source.Source, current *store.InstanceMod, update bool,
) (*stagedPackage, *jobs.Outcome) {
	// Checked here, ahead of Plan's own check: the job stages each package into a
	// directory named after it, so a full name from the index reaches the filesystem
	// here first. A name containing `..` would extract outside the staging root (B5).
	if err := installer.CheckFullName(n.FullName); err != nil {
		return nil, failed(jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.PackageInvalid.String(), Error: err.Error(),
		})
	}
	// A package whose registry never resolved would download from nowhere and be recorded
	// as coming from nowhere. Failing here keeps that from reaching disk (B14).
	if src == (source.Source{}) {
		return nil, failed(modJobFailed(errcode.Internal,
			fmt.Errorf("%s-%s resolved without a registry", n.FullName, n.Version)))
	}
	p := &stagedPackage{fullName: n.FullName, src: src, version: n.Version, transitive: n.Transitive}
	if current == nil {
		return p, nil
	}
	// A version change keeps who asked for a package: a dependency stays one, and only the
	// package an install names becomes explicit.
	if n.Transitive || update {
		p.transitive = current.InstalledAs == store.InstalledDependency
	}
	// An installed package at another version is an update: uninstall-then-install in
	// one job under one diff. The old files come off from their own manifest and the new
	// ones go on in the same commit, so there is no window with neither.
	if err := loadPrevious(p, current); err != nil {
		return nil, failed(modJobFailed(errcode.Internal, err))
	}
	return p, nil
}

// planPayload is the plan a job's payload asks for: the confirmed update targets, or one package
// with the framework and modpack rules applied.
func (i *Installer) planPayload(
	ctx context.Context, inst *store.Instance, payload *InstallPayload, idx *Index,
) (ChangePlan, error) {
	if len(payload.Updates) > 0 {
		return PlanUpdates(payload.Updates, idx)
	}
	version := payload.Version
	if installed, ok := idx.Installed(payload.FullName); ok && payload.Minimum && newer(installed, version) {
		version = installed
	}
	return i.planner().PlanInstall(ctx, inst, payload.FullName, version, idx)
}

// resolveFailure is the error code a job reports when its plan cannot be computed: moving a
// locked package is a conflict, anything else an unresolvable dependency.
func resolveFailure(err error) errcode.Code {
	var held *modresolver.HeldError
	if errors.As(err, &held) {
		return errcode.ModConflict
	}
	return errcode.DependencyUnresolved
}

// loadPrevious attaches the row an update is replacing. A manifest that will not decode
// stops the update: without an exact list of the old version's files, installing over them
// leaves orphans nothing can remove.
func loadPrevious(p *stagedPackage, current *store.InstanceMod) error {
	if current.Version == p.version {
		return nil
	}
	if err := json.Unmarshal([]byte(current.FileManifest), &p.prevManifest); err != nil {
		return fmt.Errorf("read the manifest of the installed %s-%s: %w",
			current.FullName, current.Version, err)
	}
	p.prev = current
	return nil
}

// downloadClosure fetches every package's zip through the content-addressed cache, so
// installing the same version on a second instance is a cache hit rather than a download.
func (i *Installer) downloadClosure(ctx context.Context, pkgs []*stagedPackage) error {
	for _, p := range pkgs {
		if p.remove {
			continue
		}
		zips, ok := i.Caches[p.src]
		if !ok {
			return fmt.Errorf("%s-%s resolved to the %s registry, which is not enabled",
				p.fullName, p.version, p.src)
		}
		url, size, ok, err := i.DB.ModVersionDownload(ctx, p.fullName, p.version, p.src)
		if err != nil {
			return fmt.Errorf("look up %s-%s: %w", p.fullName, p.version, err)
		}
		if !ok {
			return fmt.Errorf("%s-%s is no longer in the %s index", p.fullName, p.version, p.src)
		}
		ident := p.fullName + "-" + p.version
		path, err := zips.Get(ctx, ident, url, size)
		if err != nil {
			return fmt.Errorf("download %s: %w", ident, err)
		}
		p.zipPath = path
	}
	return nil
}

// stageClosure unpacks each zip into its own directory under the job's staging area.
// Extraction is where a third-party archive is made safe, so a failure here is the
// package's fault, not the panel's.
func stageClosure(pkgs []*stagedPackage, stagingDir string) error {
	for _, p := range pkgs {
		// A removal stages an empty directory, which is how the crash sweep finds it.
		dir := stagedPackageDir(stagingDir, p.fullName)
		if err := fsutil.MkdirAllExact(dir); err != nil {
			return fmt.Errorf("create staging for %s: %w", p.fullName, err)
		}
		if p.remove {
			continue
		}
		if err := extract.Extract(p.zipPath, dir); err != nil {
			return fmt.Errorf("unpack %s: %w", p.fullName, err)
		}
		p.stagingDir = dir
	}
	return nil
}

// planClosure turns each staged package into its placements, its pre-apply diff and its
// manifest. Claims come from what is already installed and from the packages ahead of it in this
// closure, so two packages colliding on one path is caught here rather than at write time.
func (i *Installer) planClosure(ctx context.Context, instanceID, serverRoot string, pkgs []*stagedPackage) error {
	claims, err := i.installedClaims(ctx, instanceID)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		if !p.remove {
			continue
		}
		for _, e := range p.prevManifest {
			delete(claims, e.Path)
		}
	}
	for _, p := range pkgs {
		if p.remove {
			continue
		}
		placements, err := installer.Plan(p.stagingDir, p.fullName)
		if err != nil {
			return fmt.Errorf("plan %s: %w", p.fullName, err)
		}
		changes, err := installer.Diff(p.fullName, placements, serverRoot, claims)
		if err != nil {
			return fmt.Errorf("diff %s: %w", p.fullName, err)
		}
		manifest, err := installer.Manifest(changes)
		if err != nil {
			return fmt.Errorf("hash %s: %w", p.fullName, err)
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			return fmt.Errorf("encode manifest for %s: %w", p.fullName, err)
		}
		p.changes, p.manifest, p.manifestRaw = changes, manifest, string(raw)
		p.prevStale = staleOf(p)
		for _, e := range manifest {
			claims[e.Path] = p.fullName
		}
	}
	return nil
}

// staleOf is what an update removes: paths the installed version put on disk that the new one
// does not write. Nothing under BepInEx/config/ is ever stale, since those bytes are the
// admin's and an install never overwrites them.
func staleOf(p *stagedPackage) []string {
	if p.prev == nil {
		return nil
	}
	keep := make(map[string]bool, len(p.changes))
	for _, c := range p.changes {
		keep[c.Dest] = true
	}
	var stale []string
	for _, e := range p.prevManifest {
		if !keep[e.Path] && !installer.UserConfig(e.Path) {
			stale = append(stale, e.Path)
		}
	}
	return stale
}

// installedClaims maps every path an installed package's manifest owns to that package.
func (i *Installer) installedClaims(ctx context.Context, instanceID string) (map[string]string, error) {
	installed, err := i.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	claims := map[string]string{}
	for j := range installed {
		row := &installed[j]
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(row.FileManifest), &manifest); err != nil {
			return nil, fmt.Errorf("read the manifest of %s: %w", row.FullName, err)
		}
		for _, e := range manifest {
			claims[e.Path] = row.FullName
		}
	}
	return claims, nil
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

// rollbackInstall undoes a failed install from the manifests written before any file moved, the
// same way the crash sweep does. Every package is attempted even after one fails, and only those
// that came back cleanly have their rows removed: a row whose files remain is their only
// record.
func (i *Installer) rollbackInstall(
	ctx context.Context, inst *store.Instance, payload *InstallPayload,
	pkgs []*stagedPackage, cause error,
) jobs.Outcome {
	serverRoot := serverDir(inst)
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
		return modJobFailed(errcode.Internal,
			fmt.Errorf("%w; and removing its rows failed: %w", cause, err))
	}
	if len(stuck) > 0 {
		// An install that failed is ordinary; one that could not be undone is not, and the
		// operator has to be told which packages still have files on disk.
		return modJobFailed(errcode.Internal,
			fmt.Errorf("%w; and these could not be rolled back: %s", cause, strings.Join(stuck, ", ")))
	}
	return modJobFailed(errcode.Internal, cause)
}

// planFailure maps installer's typed refusals onto error codes. A package that cannot be
// placed and a path another package owns are both answers about the request, so neither is
// a 500.
func planFailure(err error) jobs.Outcome {
	var conflict *installer.ConflictError
	if errors.As(err, &conflict) {
		return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.ModConflict.String(), Error: err.Error()}
	}
	var dup *installer.DuplicateDestError
	switch {
	case errors.As(err, &dup),
		errors.Is(err, installer.ErrInvalidFullName),
		errors.Is(err, installer.ErrUnsupportedEntry),
		errors.Is(err, installer.ErrUnsafeDest):
		return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.PackageInvalid.String(), Error: err.Error()}
	}
	return modJobFailed(errcode.Internal, err)
}

// modJobFailed is the terminal outcome every mod job reports a failure with: the error code
// for the user, and the underlying error for the operator reading the run.
func modJobFailed(code errcode.Code, err error) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: code.String(), Error: err.Error()}
}

// diffSummary is the pre-apply diff as one log line per package. Skips are counted rather
// than swallowed: a shipped config default that was not written has to be visible.
func diffSummary(p *stagedPackage) string {
	if p.remove {
		return fmt.Sprintf("%s-%s: removed, %d files deleted", p.fullName, p.version, len(p.prevStale))
	}
	var created, overwritten, skipped int
	for _, c := range p.changes {
		switch c.Action {
		case installer.ActionCreate:
			created++
		case installer.ActionOverwrite:
			overwritten++
		case installer.ActionSkip:
			skipped++
		}
	}
	name := p.fullName + "-" + p.version
	if p.prev != nil {
		name = fmt.Sprintf("%s %s -> %s", p.fullName, p.prev.Version, p.version)
	}
	return fmt.Sprintf("%s: %d new, %d replaced, %d left alone", name, created, overwritten, skipped)
}

// failed lifts a terminal Outcome into the pointer resolveForInstall returns, so "this
// phase produced an answer" is distinguishable from "carry on".
func failed(o jobs.Outcome) *jobs.Outcome { return &o }

// removedPackageOf stages an installed package a plan uninstalls. Its whole manifest is stale
// except the config files, which an uninstall always leaves.
func removedPackageOf(row *store.InstanceMod) (*stagedPackage, error) {
	p := &stagedPackage{
		fullName: row.FullName, src: row.Source, version: row.Version,
		transitive: row.InstalledAs == store.InstalledDependency, remove: true, prev: row,
		manifestRaw: "[]",
	}
	if err := json.Unmarshal([]byte(row.FileManifest), &p.prevManifest); err != nil {
		return nil, fmt.Errorf("read the manifest of the installed %s-%s: %w", row.FullName, row.Version, err)
	}
	for _, e := range p.prevManifest {
		if !installer.UserConfig(e.Path) {
			p.prevStale = append(p.prevStale, e.Path)
		}
	}
	return p, nil
}
