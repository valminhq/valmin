package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

// mod_install's checkpoints, exactly 12 §9.4's list. `↯` manifestWritten precedes any file
// moving: that ordering is what makes rollback exact rather than best-effort, and it is
// the one thing about this job that cannot be changed without changing ADR-009.
const (
	checkpointResolved        = "resolved"
	checkpointDownloaded      = "downloaded"
	checkpointStaged          = "staged"
	checkpointManifestWritten = "manifest_written"
	checkpointApplied         = "applied"
)

// BepInExPack is 03 §5.1's framework package. A modded Valheim server is this package plus
// plugins, so an instance that has no BepInEx and is about to get its first mod needs it —
// 05 M2's "BepInEx auto-install when the first mod is added to a vanilla instance".
const BepInExPack = "denikson-BepInExPack_Valheim"

// bepinexConfig is the file 03 §5.5's console-logging assertion edits, relative to server/.
const bepinexConfig = "BepInEx/config/BepInEx.cfg"

// modInstallPayload is the job's persisted arguments (12 §4.1). StagingDir is on it for the
// same reason worldImportPayload carries one: after a crash the sweep is the only thing
// left that knows where the job's staged files and its backups of what it displaced are.
type modInstallPayload struct {
	StagingDir string `json:"staging_dir"`
	FullName   string `json:"full_name"`
	Version    string `json:"version"`
}

// modStagingRoot is where a mod install stages extracted packages and backs up whatever it
// displaces. Under data.root beside the import staging area, so the move into server/ is a
// rename on one filesystem rather than a second copy.
func modStagingRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "staging", "mods")
}

func stagedPackageDir(stagingDir, fullName string) string {
	return filepath.Join(stagingDir, "pkg", fullName)
}

func stagingBackupDir(stagingDir string) string { return filepath.Join(stagingDir, "backup") }

// installMods is POST /instances/{id}/mods (04 §3): resolve, download, place, and record a
// manifest per package. It answers 202 and a job (11 §3) — nothing about this is fast
// enough to hold a request open, and ADR-028 wants one reliability story for every
// mutation regardless of how long it takes.
func (m *Mods) installMods(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	// `↯` B11 / C19: mods are applied to a stopped server, and a job never implicitly stops
	// one. A running instance is refused and keeps running.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	body, ok := decodePackageRequest(w, r)
	if !ok {
		return
	}

	job, err := m.submitInstall(r.Context(), inst, body, u.ID, nil)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// CheckResolvable is ModEngine's half of 04 §3's resolve-before-install, for a server that
// does not exist yet. It computes exactly the closure the install will and throws it away —
// the answer wanted here is only whether the index can produce one.
//
// `↯` It exists so that "one of the mods you picked cannot be resolved" is an answer to the
// create *request*, not a job that fails after a 1 GB game download has already run. The
// wizard's user is told before anything is provisioned, which is the same ordering the mod
// screen's confirm dialog gives on an existing server.
func (m *Mods) CheckResolvable(ctx context.Context, inst *store.Instance, req resolveRequest) error {
	idx := &storeIndex{ctx: ctx, db: m.DB, instanceID: inst.ID}
	_, resolveErr := m.resolveClosure(ctx, inst, req.FullName, req.Version, idx)
	if idx.err != nil {
		return idx.err
	}
	return resolveErr
}

// SubmitInstall is ModEngine's install verb: submitInstall with the job discarded, because
// the create chain follows the work through afterFinish rather than through a job id.
func (m *Mods) SubmitInstall(
	ctx context.Context,
	inst *store.Instance,
	req resolveRequest,
	requestedBy string,
	afterFinish func(context.Context),
) error {
	_, err := m.submitInstall(ctx, inst, req, requestedBy, afterFinish)
	return err
}

// submitInstall stages a directory for one package and submits its mod_install job.
//
// Extracted from installMods so the create wizard's chain (provision.go) submits mods the
// same way the mod screen does — one path, so B11's compare-and-swap, the staging-directory
// cleanup and the runner cannot be got subtly wrong on a second one. afterFinish is the
// chain's continuation and is nil for the handler.
func (m *Mods) submitInstall(
	ctx context.Context,
	inst *store.Instance,
	req resolveRequest,
	requestedBy string,
	afterFinish func(context.Context),
) (*store.Job, error) {
	root := modStagingRoot(m.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		return nil, fmt.Errorf("create the mod staging root: %w", err)
	}
	staging, err := os.MkdirTemp(root, "install-*")
	if err != nil {
		return nil, fmt.Errorf("create a staging directory for %s: %w", req.FullName, err)
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	id := inst.ID
	payload := modInstallPayload{StagingDir: staging, FullName: req.FullName, Version: req.Version}
	job, err := m.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModInstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: payload,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			// A stopped→stopped compare-and-swap, as world_import does: the kind holds the
			// lock without moving the state, and the CAS is what makes "still stopped when
			// the lock was taken" atomic with taking it.
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateStopped))
			if err != nil {
				return fmt.Errorf("claim mod_install for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, m.runModInstallThen(inst, payload, afterFinish))
	if err != nil {
		// Not wrapped: writeJobSubmitError reads the typed conflicts out of this with
		// errors.As, and the caller in provision.go only logs it.
		return nil, err //nolint:wrapcheck // the caller matches on the engine's typed errors
	}
	submitted = true
	return job, nil
}

// runModInstallThen is runModInstall with a continuation hung off a *successful* outcome.
//
// `↯` The continuation runs only when the install succeeded. A chain that started the
// server after a failed install would hand the operator a running vanilla world and call it
// done — and the world is written on that first boot, which is the one step this whole
// feature exists to get in front of (Q42).
func (m *Mods) runModInstallThen(
	inst *store.Instance, payload modInstallPayload, afterFinish func(context.Context),
) jobs.Runner {
	run := m.runModInstall(inst, payload)
	if afterFinish == nil {
		return run
	}
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		out := run(ctx, h)
		if out.Status == "succeeded" {
			out.AfterFinish = afterFinish
		}
		return out
	}
}

// installedModView is one row of GET /instances/{id}/mods (04 §3). The file manifest is
// deliberately not on it: it is an implementation detail of uninstall, often thousands of
// paths long, and no screen renders it.
type installedModView struct {
	FullName string `json:"full_name"`
	// Namespace and Name are the package's author and its own name, carried separately so
	// a screen can render "Warfare, by Therzie" rather than the ident "Therzie-Warfare".
	//
	// `↯` They are read from the synced catalogue, never split out of FullName. 03 §6.2
	// names the format "Namespace-Name" but nothing forbids a hyphen inside either half —
	// resolver.ParseDependency already anchors on the version's shape for exactly this
	// reason — so a split on the first hyphen is a guess, and it is the frontend that would
	// be guessing. Empty when the catalogue holds no row for the package: never synced, or
	// removed upstream since it was installed. A caller with nothing to show falls back to
	// FullName, which is why these are empty strings rather than an error.
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	InstalledAs string `json:"installed_as"`
	Side        string `json:"side"`
	Enabled     bool   `json:"enabled"`
	InstalledAt string `json:"installed_at"`
	FileCount   int    `json:"file_count"`
	// LoadStatus is 05 M2's load verification, per mod. Null means the panel has nothing to
	// compare against — no BepInEx log yet, or a package that places no plugin at all — and
	// is deliberately distinct from LoadNotSeen, which is an observation.
	LoadStatus *string `json:"load_status"`
}

// Load statuses on installedModView.
//
// `↯` There is no `failed` here, and its absence is deliberate. 03 §5.3 says per-plugin
// failures appear in `LogOutput.log`, but the pack records no measured literal for one —
// only the successful `Loading [...]` line was captured (M0, 20 Aug 2026). Inventing the
// error pattern is exactly what CLAUDE.md §9 forbids, and a wrong one reports healthy mods
// as broken. `not_seen` is the honest superset until it is measured (Q38).
const (
	LoadLoaded  = "loaded"
	LoadNotSeen = "not_seen"
)

// pluginLoadView is the boot-level half of load verification: what BepInEx said it was
// going to load, what it actually named, and whether those two disagree (03 §5.3).
type pluginLoadView struct {
	ObservedAt string `json:"observed_at"`
	// Declared is the count line's number, null when the run printed none.
	Declared *int `json:"declared"`
	Loaded   int  `json:"loaded"`
	// Discrepancy is null when the two agree. It is reported rather than resolved: the
	// gap between them is a plugin BepInEx meant to load and never named.
	Discrepancy *string `json:"discrepancy"`
}

// listInstalledMods is GET /instances/{id}/mods, gated on mods.list — a viewer capability
// (09 §3.1), so anyone who can see the instance can see what is on it.
func (m *Mods) listInstalledMods(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}

	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	mods, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	// `↯` A log the panel cannot read costs the load statuses and nothing else. The list of
	// what is installed comes from the database and is still correct; failing the whole
	// page over BepInEx's log file would take away the screen an admin reaches for when
	// their mods are not working.
	load, err := instance.ReadPluginLoad(inst.DataDir)
	if err != nil {
		slog.WarnContext(r.Context(), "could not read the BepInEx log for load verification",
			slog.String("instance_id", id), slog.Any("error", err))
	}

	views := make([]installedModView, 0, len(mods))
	for i := range mods {
		// One primary-key lookup per installed package, for the author and display name.
		// An instance holds tens of mods, not thousands, so this stays a handful of cheap
		// reads; batch it if that ever stops being true. A miss is not an error — see the
		// note on installedModView.Namespace.
		pkg, err := m.DB.ModPackageByFullName(r.Context(), mods[i].FullName)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		views = append(views, toInstalledModView(&mods[i], pkg, load))
	}
	JSON(w, r, http.StatusOK, map[string]any{"mods": views, "plugin_load": toPluginLoadView(load)})
}

func toInstalledModView(m *store.InstanceMod, pkg *store.ModPackage, load *instance.PluginLoad) installedModView {
	var manifest []installer.ManifestEntry
	// A manifest that will not decode is a row this panel wrote and something later broke.
	// It costs the file count and nothing else, so the row is still listed — a mod the user
	// can see and uninstall beats a 500 on the whole page.
	_ = json.Unmarshal([]byte(m.FileManifest), &manifest)
	var namespace, name string
	if pkg != nil {
		namespace, name = pkg.Namespace, pkg.Name
	}
	return installedModView{
		FullName: m.FullName, Namespace: namespace, Name: name,
		Version: m.Version, InstalledAs: m.InstalledAs,
		Side: m.Side, Enabled: m.Enabled, InstalledAt: m.InstalledAt, FileCount: len(manifest),
		LoadStatus: loadStatus(m.FullName, manifest, load),
	}
}

// loadStatus is one mod's answer to "did this actually load". Null — no answer — is the
// result for a package that places no plugin, and for every package when there is no
// chainloader run to read: an admin who has not started the server since installing a mod
// must not be told it is not loading.
func loadStatus(
	fullName string, manifest []installer.ManifestEntry, load *instance.PluginLoad,
) *string {
	if load == nil {
		return nil
	}
	paths := installer.Paths(manifest)
	if !instance.IsPlugin(paths) {
		return nil
	}
	status := LoadNotSeen
	if load.Loaded(fullName, paths) {
		status = LoadLoaded
	}
	return &status
}

func toPluginLoadView(load *instance.PluginLoad) *pluginLoadView {
	if load == nil {
		return nil
	}
	view := pluginLoadView{
		ObservedAt: load.ObservedAt.UTC().Format(time.RFC3339),
		Loaded:     len(load.Plugins),
	}
	if load.Declared >= 0 {
		declared := load.Declared
		view.Declared = &declared
	}
	if d := load.Discrepancy(); d != "" {
		view.Discrepancy = &d
	}
	return &view
}

// modInstallCancelPolicy is 12 §8's row for this kind: cancellable until staged files begin
// moving into server/. The move starts once manifest_written is recorded, so that
// checkpoint and everything after it is past the point of no return — from there the
// manifest rollback path owns the outcome, and a half-honoured cancellation would be a
// worse answer than a refusal.
func modInstallCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case checkpointManifestWritten, checkpointApplied:
		return false, "placing files into the server directory"
	default:
		return true, ""
	}
}

// stagedPackage is one package of the closure, carried between the runner's phases.
type stagedPackage struct {
	fullName    string
	version     string
	transitive  bool
	zipPath     string
	stagingDir  string
	changes     []installer.Change
	manifest    []installer.ManifestEntry
	manifestRaw string
	// prev is the row this package is replacing, on an update, and nil on a first install.
	// It carries the old version's file manifest, which is the only exact record of what
	// that version put on disk — 03 §6.4's heuristics describe the package as it is
	// published today, not as it was installed.
	prev *store.InstanceMod
	// prevManifest is prev's decoded manifest, and prevStale the part of it the new version
	// does not also write. Those are the files an update removes; the rest are overwritten
	// in place by Apply.
	prevManifest []installer.ManifestEntry
	prevStale    []string
}

// prevRoot is where an update saves the rows it is about to overwrite. `↯` It exists
// because the database cannot hold them: the update rewrites each row in place before any
// file moves (12 §9.4), so from the manifest_written checkpoint onward the only record of
// what was installed a moment ago is this directory. Without it a crash mid-update restores
// the old files from the backup and leaves no row naming them — B9's orphan, produced by
// the rollback rather than prevented by it.
func prevRowDir(stagingDir string) string { return filepath.Join(stagingDir, "prev") }

func prevRowPath(stagingDir, fullName string) string {
	return filepath.Join(prevRowDir(stagingDir), fullName+".json")
}

// replaced is what an update saves about the version it is overwriting: the row itself, and
// the exact set of files it is about to remove.
//
// `↯` Stale is recorded rather than re-derived. The sweep could otherwise take it to be the
// whole old manifest minus the new one — which quietly includes the config files the diff
// *skipped*, and would have the crash recovery delete the user's settings that the install
// itself was careful to leave alone.
type replaced struct {
	Row   store.InstanceMod `json:"row"`
	Stale []string          `json:"stale"`
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

// readPrevRow is the sweep's half of writePrevRows: what an interrupted update had
// replaced, or nil if this package was a fresh install.
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

// rollbackEntries is every path a failed install has to undo for one package: what it was
// going to write, plus what it removed to make room. The two are disjoint by construction —
// stale is the old manifest minus everything the new diff touches — so the union names each
// path once, and Rollback restores the ones with a backup and deletes the rest.
func rollbackEntries(manifest []installer.ManifestEntry, stale []string) []installer.ManifestEntry {
	out := make([]installer.ManifestEntry, 0, len(manifest)+len(stale))
	out = append(out, manifest...)
	for _, path := range stale {
		out = append(out, installer.ManifestEntry{Path: path})
	}
	return out
}

// runModInstall is the mod_install Runner. The phases are 12 §9.4's checkpoints and the
// order is load-bearing: resolve, download, stage, **write the manifest**, then move files.
// Every failure at or after the manifest is undone from the manifest itself, which is the
// only record that is exact — a half-applied server/ is not.
func (m *Mods) runModInstall(inst *store.Instance, payload modInstallPayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()

		pkgs, outcome := m.prepareInstall(ctx, h, inst, payload)
		if outcome != nil {
			return *outcome
		}
		if len(pkgs) == 0 {
			h.Progress(ctx, 100, "already installed; nothing to do")
			return jobs.Outcome{Status: "succeeded"}
		}
		return m.commitInstall(ctx, h, inst, payload, pkgs)
	}
}

// prepareInstall is everything that can still be abandoned: resolve, download, unpack, and
// work out what would change. Nothing it does is visible in server/ or in the database, so
// a failure or a cancellation here needs no undoing beyond deleting the staging directory.
func (m *Mods) prepareInstall(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, payload modInstallPayload,
) ([]*stagedPackage, *jobs.Outcome) {
	h.Progress(ctx, 5, "resolving dependencies")
	pkgs, outcome := m.resolveForInstall(ctx, inst, payload)
	if outcome != nil {
		return nil, outcome
	}
	if outcome := mark(ctx, h, checkpointResolved); outcome != nil {
		return nil, outcome
	}
	if len(pkgs) == 0 {
		return nil, nil
	}

	h.Progress(ctx, 20, fmt.Sprintf("downloading %d packages", len(pkgs)))
	if err := m.downloadClosure(ctx, pkgs); err != nil {
		return nil, failed(modJobFailed(apierr.Unavailable, err))
	}
	if outcome := mark(ctx, h, checkpointDownloaded); outcome != nil {
		return nil, outcome
	}

	h.Progress(ctx, 45, "unpacking")
	if err := stageClosure(pkgs, payload.StagingDir); err != nil {
		return nil, failed(modJobFailed(apierr.PackageInvalid, err))
	}
	if outcome := mark(ctx, h, checkpointStaged); outcome != nil {
		return nil, outcome
	}

	h.Progress(ctx, 60, "checking what would change")
	if err := m.planClosure(ctx, inst.ID, serverDir(inst), pkgs); err != nil {
		return nil, failed(planFailure(err))
	}
	for _, p := range pkgs {
		h.Log(diffSummary(p))
	}
	return pkgs, nil
}

// commitInstall is the half that changes things: back up what the whole closure would
// displace, record the manifests, then move the files.
//
// `↯` The backup pass covers every package before the first manifest row is written.
// Rollback reads "this manifest path has no backup" as "this path is ours, delete it", and
// that is only true if everything the install could displace was saved first — backing up
// inside each package's own apply would leave the later packages' manifests recorded with
// nothing behind them, and a crash would then delete operator files the install never
// touched.
//
// `↯` The cancellation check is the last one this job makes (12 §8). Past the manifest,
// files are moving and the rollback path owns the outcome, so the API refuses a cancel from
// there rather than half-honouring it.
func (m *Mods) commitInstall(
	ctx context.Context, h *jobs.Handle, inst *store.Instance,
	payload modInstallPayload, pkgs []*stagedPackage,
) jobs.Outcome {
	if h.CancelRequested(ctx) {
		return jobs.Outcome{Status: "cancelled"}
	}

	serverRoot := serverDir(inst)
	backupDir := stagingBackupDir(payload.StagingDir)

	h.Progress(ctx, 68, "saving what would be replaced")
	for _, p := range pkgs {
		if err := installer.Backup(p.changes, serverRoot, backupDir); err != nil {
			// Nothing is recorded and nothing has moved, so there is nothing to undo.
			return modJobFailed(apierr.Internal, fmt.Errorf("back up for %s: %w", p.fullName, err))
		}
		// An update's stale files are displaced just as surely as an overwritten one, and
		// they are saved in the same pass and before the same checkpoint, for the same
		// reason: the rollback's only rule is "restore what has a backup".
		if err := installer.BackupPaths(p.prevStale, serverRoot, backupDir); err != nil {
			return modJobFailed(apierr.Internal,
				fmt.Errorf("back up what %s replaces: %w", p.fullName, err))
		}
	}
	if err := writePrevRows(payload.StagingDir, pkgs); err != nil {
		return modJobFailed(apierr.Internal, err)
	}

	h.Progress(ctx, 70, "recording the file manifest")
	if err := m.writeManifests(ctx, inst.ID, pkgs); err != nil {
		return modJobFailed(apierr.Internal, err)
	}

	// From here every failure goes through the rollback, including a checkpoint that will
	// not write: the rows and restart_required are already committed, and a job that ends
	// terminal without undoing them leaves an install the sweep will never look at and a
	// retry will treat as already done.
	if err := h.Checkpoint(ctx, checkpointManifestWritten); err != nil {
		return m.rollbackInstall(ctx, inst, payload, pkgs, err)
	}

	h.Progress(ctx, 85, "placing files")
	for _, p := range pkgs {
		// The old version's files come off first, from its own manifest (B9), so what the
		// new version does not ship cannot survive as an orphan BepInEx would still load.
		if err := installer.Remove(p.prevStale, serverRoot); err != nil {
			return m.rollbackInstall(ctx, inst, payload, pkgs,
				fmt.Errorf("remove the replaced files of %s: %w", p.fullName, err))
		}
		if err := installer.Apply(p.changes, serverRoot); err != nil {
			return m.rollbackInstall(ctx, inst, payload, pkgs, fmt.Errorf("apply %s: %w", p.fullName, err))
		}
	}
	if err := h.Checkpoint(ctx, checkpointApplied); err != nil {
		return m.rollbackInstall(ctx, inst, payload, pkgs, err)
	}

	h.Progress(ctx, 100, fmt.Sprintf("installed %d packages", len(pkgs)))
	return jobs.Outcome{
		Status:   "succeeded",
		OnFinish: markModded(inst.ID, m.installedBepInEx(ctx, inst, pkgs)),
		// `↯` The console key is flipped only once the install has actually committed.
		// Inside the job it would be a byte the rollback does not know about — it is not in
		// any manifest, because an install never overwrites an existing config — so a
		// crash between the edit and the commit would undo every file yet leave the
		// operator's deliberate `false` turned on.
		AfterFinish: func(ctx context.Context) { m.ensureConsoleLogging(ctx, serverRoot, pkgs) },
	}
}

// installedBepInEx is the framework version this instance ends up running, or "" if it is
// not modded at all. It falls back to the installed row rather than only reading this
// closure: a package already present at a satisfying version is a no-op and never appears
// in pkgs, so an instance whose modded flag was somehow never set would otherwise stay
// unflagged forever — and E1 is skipped on an instance that is not marked modded.
func (m *Mods) installedBepInEx(ctx context.Context, inst *store.Instance, pkgs []*stagedPackage) string {
	if version := versionOf(pkgs, BepInExPack); version != "" {
		return version
	}
	if inst.Modded {
		return ""
	}
	version, ok, err := m.DB.InstanceModVersion(ctx, inst.ID, BepInExPack)
	if err != nil || !ok {
		return ""
	}
	return version
}

// ensureConsoleLogging is 03 §5.5's assertion, run only when this install placed the
// framework package. A file the panel cannot make say `true` is a warning on the job, never
// a failure: the server runs fine, the panel just cannot read its plugin lines — and the E1
// startup assertion is the backstop that says so out loud at boot.
func (m *Mods) ensureConsoleLogging(ctx context.Context, serverRoot string, pkgs []*stagedPackage) {
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

// markModded records that this instance now runs BepInEx (ADR-019). It lands in the job's
// own Finish transaction from data already in memory (12 §6), so it is written once the
// files are actually on disk and never survives a rollback.
func markModded(instanceID, version string) func(context.Context, *sql.Tx) error {
	if version == "" {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		return store.TxSetModded(ctx, tx, instanceID, version)
	}
}

func versionOf(pkgs []*stagedPackage, fullName string) string {
	for _, p := range pkgs {
		if p.fullName == fullName {
			return p.version
		}
	}
	return ""
}

// mark records a checkpoint, returning the terminal outcome if it could not be written — a
// job whose resume marker is not on the row is one the crash sweep would misread.
func mark(ctx context.Context, h *jobs.Handle, checkpoint string) *jobs.Outcome {
	if err := h.Checkpoint(ctx, checkpoint); err != nil {
		return failed(modJobFailed(apierr.Internal, err))
	}
	return nil
}

// serverDir is 02 §3's server/ under an instance's data directory — the only tree a mod
// install writes to.
func serverDir(inst *store.Instance) string { return filepath.Join(inst.DataDir, "server") }

// resolveForInstall computes the closure and drops the nodes that need no work. A nil
// outcome means the packages returned are the ones to install; a non-nil one is the
// terminal answer.
// resolveClosure is what an install would place, and what the resolve dry run previews —
// one function, because 04 §3's whole reason for a dry run is that the user confirms the
// closure *before* anything downloads. Two code paths would eventually disagree, and the
// preview is the one nobody notices going stale.
func (m *Mods) resolveClosure(
	ctx context.Context, inst *store.Instance, fullName, version string, idx *storeIndex,
) (modresolver.Closure, error) {
	requests := []modresolver.Request{{FullName: fullName, Version: version}}
	closure, err := modresolver.Resolve(requests, idx)
	if idx.err != nil {
		// A store read failed, so the verdict is worthless. The caller checks idx.err before
		// trusting any of this, and reporting the resolver's own error here would surface a
		// database hiccup to the user as dependency_unresolved — a lie about their request.
		return closure, nil //nolint:nilerr // idx.err is the real failure, and the caller reads it
	}
	if err != nil {
		return closure, fmt.Errorf("resolve %s-%s: %w", fullName, version, err)
	}
	if !inst.Modded && !hasNode(closure, BepInExPack) {
		if closure, err = m.withBepInEx(ctx, inst.ID, requests, idx); err != nil || idx.err != nil {
			return closure, err
		}
	}
	return markTransitive(closure, fullName), nil
}

// markTransitive restates what came along for the ride. `↯` The resolver derives Transitive
// from "was this named in the requests", and the framework package is added *to* the
// requests — so it would come back marked explicit, and the confirm dialog would tell the
// user they asked for BepInEx. Exactly one package is ever requested here; everything else
// is a dependency, auto-installed framework included.
func markTransitive(closure modresolver.Closure, requested string) modresolver.Closure {
	for i := range closure.Nodes {
		closure.Nodes[i].Transitive = closure.Nodes[i].FullName != requested
	}
	return closure
}

func (m *Mods) resolveForInstall(
	ctx context.Context, inst *store.Instance, payload modInstallPayload,
) ([]*stagedPackage, *jobs.Outcome) {
	instanceID := inst.ID
	idx := &storeIndex{ctx: ctx, db: m.DB, instanceID: instanceID}
	closure, resolveErr := m.resolveClosure(ctx, inst, payload.FullName, payload.Version, idx)
	if idx.err != nil {
		return nil, failed(modJobFailed(apierr.Internal, idx.err))
	}
	if resolveErr != nil {
		return nil, failed(modJobFailed(apierr.DependencyUnresolved, resolveErr))
	}

	installed, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, failed(modJobFailed(apierr.Internal, err))
	}
	have := make(map[string]*store.InstanceMod, len(installed))
	for i := range installed {
		have[installed[i].FullName] = &installed[i]
	}

	var out []*stagedPackage
	for _, n := range closure.Nodes {
		if n.NoOp {
			continue
		}
		// `↯` B5, and earlier than Plan's own check: the job stages each package into a
		// directory named after it, so a full name from the index reaches the filesystem
		// here first. A name with `..` in it would extract a third-party zip outside the
		// staging root.
		if err := installer.CheckFullName(n.FullName); err != nil {
			return nil, failed(jobs.Outcome{
				Status: "failed", ErrorCode: apierr.PackageInvalid.String(), Error: err.Error(),
			})
		}
		p := &stagedPackage{fullName: n.FullName, version: n.Version, transitive: n.Transitive}
		// An installed package at another version is an update, and the plan's shape for it
		// is uninstall-then-install *in one job, under one diff*: the old version's files
		// come off from its own manifest, which is the only exact record of them, and the
		// new version goes on in the same commit. Two jobs would leave a window in which the
		// server has neither.
		if current, ok := have[n.FullName]; ok {
			if err := loadPrevious(p, current); err != nil {
				return nil, failed(modJobFailed(apierr.Internal, err))
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// loadPrevious attaches the row an update is replacing. A manifest that will not decode
// stops the update: without it there is no exact list of the old version's files, and
// installing over them anyway is how the orphan DLLs ADR-009 exists to prevent get made.
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

// withBepInEx re-resolves the closure with the framework package added — 05 M2's
// "BepInEx auto-install when the first mod is added to a vanilla instance".
//
// `↯` It runs only when the closure does not already name the package. Adding it
// unconditionally would make its *latest* version a request, and 03 §6.3 resolves a diamond
// upward — so a mod that pins BepInEx 5.4.2333 would silently get whatever the index calls
// latest today, which is a version bump nobody asked for.
func (m *Mods) withBepInEx(
	ctx context.Context, instanceID string, requests []modresolver.Request, idx *storeIndex,
) (modresolver.Closure, error) {
	version, ok, err := m.bepinexVersion(ctx, instanceID)
	if err != nil {
		return modresolver.Closure{}, err
	}
	if !ok {
		return modresolver.Closure{}, &modresolver.UnresolvedError{FullName: BepInExPack, Version: "latest"}
	}
	closure, err := modresolver.Resolve(
		append(requests, modresolver.Request{FullName: BepInExPack, Version: version}), idx)
	if err != nil {
		return closure, fmt.Errorf("resolve %s with %s: %w", BepInExPack, version, err)
	}
	return closure, nil
}

// hasNode reports whether a resolved closure already names fullName.
func hasNode(closure modresolver.Closure, fullName string) bool {
	for _, n := range closure.Nodes {
		if n.FullName == fullName {
			return true
		}
	}
	return false
}

// bepinexVersion is the framework version to auto-install: whatever the cached index calls
// latest. ok is false when no sync has ever seen the package, which is not a panel fault —
// it is a panel that has never reached Thunderstore.
func (m *Mods) bepinexVersion(ctx context.Context, instanceID string) (version string, ok bool, err error) {
	if version, ok, err := m.DB.InstanceModVersion(ctx, instanceID, BepInExPack); err != nil {
		return "", false, fmt.Errorf("read the installed framework version: %w", err)
	} else if ok {
		return version, true, nil
	}
	pkg, err := m.DB.ModPackageByFullName(ctx, BepInExPack)
	if err != nil {
		return "", false, fmt.Errorf("look up %s: %w", BepInExPack, err)
	}
	if pkg == nil || pkg.LatestVersion == "" {
		return "", false, nil
	}
	return pkg.LatestVersion, true, nil
}

// downloadClosure fetches every package's zip through the content-addressed cache, so
// installing the same version on a second instance is a cache hit rather than a download
// (03 §6.1).
func (m *Mods) downloadClosure(ctx context.Context, pkgs []*stagedPackage) error {
	for _, p := range pkgs {
		url, size, ok, err := m.DB.ModVersionDownload(ctx, p.fullName, p.version)
		if err != nil {
			return fmt.Errorf("look up %s-%s: %w", p.fullName, p.version, err)
		}
		if !ok {
			return fmt.Errorf("%s-%s is no longer in the cached index", p.fullName, p.version)
		}
		ident := p.fullName + "-" + p.version
		path, err := m.Cache.Get(ctx, ident, url, size)
		if err != nil {
			return fmt.Errorf("download %s: %w", ident, err)
		}
		p.zipPath = path
	}
	return nil
}

// stageClosure unpacks each zip into its own directory under the job's staging area.
// Extraction is where an arbitrary third-party archive is made safe (03 §6.5), so a
// failure here is the package's fault, not the panel's.
func stageClosure(pkgs []*stagedPackage, stagingDir string) error {
	for _, p := range pkgs {
		dir := stagedPackageDir(stagingDir, p.fullName)
		if err := fsutil.MkdirAllExact(dir); err != nil {
			return fmt.Errorf("create staging for %s: %w", p.fullName, err)
		}
		if err := extract.Extract(p.zipPath, dir); err != nil {
			return fmt.Errorf("unpack %s: %w", p.fullName, err)
		}
		p.stagingDir = dir
	}
	return nil
}

// planClosure turns each staged package into its placements, its pre-apply diff and its
// manifest. Claims come from what is already installed *and* from the packages ahead of it
// in this same closure, so two packages of one install colliding on a path is caught here
// rather than by whichever wrote second.
func (m *Mods) planClosure(ctx context.Context, instanceID, serverRoot string, pkgs []*stagedPackage) error {
	claims, err := m.installedClaims(ctx, instanceID)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
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

// staleOf is what an update removes: the paths the installed version put on disk that the
// new one does not write.
//
// `↯` Nothing under BepInEx/config/ is ever stale, whether or not the new version still
// ships it. An install never overwrites a config file (03 §6.4), so the bytes at that path
// are the admin's, and removing them because the old manifest happens to name the file
// would turn "we never overwrite your settings" into "we deleted them instead".
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
func (m *Mods) installedClaims(ctx context.Context, instanceID string) (map[string]string, error) {
	installed, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	claims := map[string]string{}
	for i := range installed {
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(installed[i].FileManifest), &manifest); err != nil {
			return nil, fmt.Errorf("read the manifest of %s: %w", installed[i].FullName, err)
		}
		for _, e := range manifest {
			claims[e.Path] = installed[i].FullName
		}
	}
	return claims, nil
}

// writeManifests records every package's rows and marks the instance as needing a restart,
// in one transaction (12 §6: the flip is transactional, the work that produced it was not).
func (m *Mods) writeManifests(ctx context.Context, instanceID string, pkgs []*stagedPackage) error {
	rows := make([]store.InstanceMod, 0, len(pkgs))
	for _, p := range pkgs {
		installedAs := store.InstalledExplicit
		if p.transitive {
			installedAs = store.InstalledDependency
		}
		rows = append(rows, store.InstanceMod{
			InstanceID: instanceID, FullName: p.fullName, Version: p.version,
			InstalledAs: installedAs, Side: store.SideUnknown, Enabled: true,
			FileManifest: p.manifestRaw,
		})
	}
	if err := m.DB.WriteInstanceMods(ctx, instanceID, rows); err != nil {
		return fmt.Errorf("record the file manifests: %w", err)
	}
	return nil
}

// rollbackInstall undoes a failed install from the manifests written before any file moved
// (ADR-009, 12 §9.4). It is deliberately the same shape as the crash sweep's rollback: one
// path, so there is not a second one to get wrong.
//
// Every package is attempted even after one fails, and only the ones that came back cleanly
// have their rows removed — a row whose files are still on disk is the only record of them,
// and deleting it would strand them.
func (m *Mods) rollbackInstall(
	ctx context.Context, inst *store.Instance, payload modInstallPayload,
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
	if err := m.DB.RollbackInstanceMods(ctx, inst.ID, restore, rolled); err != nil {
		return modJobFailed(apierr.Internal,
			fmt.Errorf("%w; and removing its rows failed: %w", cause, err))
	}
	if len(stuck) > 0 {
		// An install that failed is ordinary; one that could not be undone is not, and the
		// operator has to be told which packages still have files on disk.
		return modJobFailed(apierr.Internal,
			fmt.Errorf("%w; and these could not be rolled back: %s", cause, strings.Join(stuck, ", ")))
	}
	return modJobFailed(apierr.Internal, cause)
}

// planFailure maps installer's typed refusals onto the registry. A package the panel cannot
// place and a path another package owns are both answers about the request, not faults of
// the panel, so neither is a 500.
func planFailure(err error) jobs.Outcome {
	var conflict *installer.ConflictError
	if errors.As(err, &conflict) {
		return jobs.Outcome{Status: "failed", ErrorCode: apierr.ModConflict.String(), Error: err.Error()}
	}
	var dup *installer.DuplicateDestError
	switch {
	case errors.As(err, &dup),
		errors.Is(err, installer.ErrInvalidFullName),
		errors.Is(err, installer.ErrUnsupportedEntry),
		errors.Is(err, installer.ErrUnsafeDest):
		return jobs.Outcome{Status: "failed", ErrorCode: apierr.PackageInvalid.String(), Error: err.Error()}
	}
	return modJobFailed(apierr.Internal, err)
}

// modJobFailed is the terminal outcome both mod jobs report a failure with: the registry
// code plus what actually went wrong, since 11 §2's message is for the user and the job's
// error is for the operator reading the run.
func modJobFailed(code apierr.Code, err error) jobs.Outcome {
	return jobs.Outcome{Status: "failed", ErrorCode: code.String(), Error: err.Error()}
}

// diffSummary is the pre-apply diff as one log line per package (02 §4.2). A skip is
// counted here rather than swallowed: 03 §6.4 requires a shipped config default that was
// not written to be visible.
func diffSummary(p *stagedPackage) string {
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
	return fmt.Sprintf("%s-%s: %d new, %d replaced, %d left alone",
		p.fullName, p.version, created, overwritten, skipped)
}

// failed lifts a terminal Outcome into the pointer resolveForInstall returns, so "this
// phase produced an answer" is distinguishable from "carry on".
func failed(o jobs.Outcome) *jobs.Outcome { return &o }

// decodePackageRequest reads the {full_name, version} body both resolve and install take,
// answering 422 with the field errors if either is missing.
func decodePackageRequest(w http.ResponseWriter, r *http.Request) (resolveRequest, bool) {
	var body resolveRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	var val apierr.Validation
	if strings.TrimSpace(body.FullName) == "" {
		val.Add("full_name", apierr.FieldRequired, "full_name is required.")
	}
	if strings.TrimSpace(body.Version) == "" {
		val.Add("version", apierr.FieldRequired, "version is required.")
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	return body, true
}
