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

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
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

	root := modStagingRoot(m.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	staging, err := os.MkdirTemp(root, "install-*")
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	payload := modInstallPayload{StagingDir: staging, FullName: body.FullName, Version: body.Version}
	job, err := m.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindModInstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID, Payload: payload,
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
	}, m.runModInstall(inst, payload))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// installedModView is one row of GET /instances/{id}/mods (04 §3). The file manifest is
// deliberately not on it: it is an implementation detail of uninstall, often thousands of
// paths long, and no screen renders it.
type installedModView struct {
	FullName    string `json:"full_name"`
	Version     string `json:"version"`
	InstalledAs string `json:"installed_as"`
	Side        string `json:"side"`
	Enabled     bool   `json:"enabled"`
	InstalledAt string `json:"installed_at"`
	FileCount   int    `json:"file_count"`
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

	mods, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	views := make([]installedModView, 0, len(mods))
	for i := range mods {
		views = append(views, toInstalledModView(&mods[i]))
	}
	JSON(w, r, http.StatusOK, map[string]any{"mods": views})
}

func toInstalledModView(m *store.InstanceMod) installedModView {
	var manifest []installer.ManifestEntry
	// A manifest that will not decode is a row this panel wrote and something later broke.
	// It costs the file count and nothing else, so the row is still listed — a mod the user
	// can see and uninstall beats a 500 on the whole page.
	_ = json.Unmarshal([]byte(m.FileManifest), &manifest)
	return installedModView{
		FullName: m.FullName, Version: m.Version, InstalledAs: m.InstalledAs,
		Side: m.Side, Enabled: m.Enabled, InstalledAt: m.InstalledAt, FileCount: len(manifest),
	}
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
	pkgs, outcome := m.resolveForInstall(ctx, inst.ID, payload)
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
		return nil, failed(modInstallFailed(apierr.Unavailable, err))
	}
	if outcome := mark(ctx, h, checkpointDownloaded); outcome != nil {
		return nil, outcome
	}

	h.Progress(ctx, 45, "unpacking")
	if err := stageClosure(pkgs, payload.StagingDir); err != nil {
		return nil, failed(modInstallFailed(apierr.PackageInvalid, err))
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
			return modInstallFailed(apierr.Internal, fmt.Errorf("back up for %s: %w", p.fullName, err))
		}
	}

	h.Progress(ctx, 70, "recording the file manifest")
	if err := m.writeManifests(ctx, inst.ID, pkgs); err != nil {
		return modInstallFailed(apierr.Internal, err)
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
		if err := installer.Apply(p.changes, serverRoot); err != nil {
			return m.rollbackInstall(ctx, inst, payload, pkgs, fmt.Errorf("apply %s: %w", p.fullName, err))
		}
	}
	if err := h.Checkpoint(ctx, checkpointApplied); err != nil {
		return m.rollbackInstall(ctx, inst, payload, pkgs, err)
	}

	h.Progress(ctx, 100, fmt.Sprintf("installed %d packages", len(pkgs)))
	return jobs.Outcome{Status: "succeeded"}
}

// mark records a checkpoint, returning the terminal outcome if it could not be written — a
// job whose resume marker is not on the row is one the crash sweep would misread.
func mark(ctx context.Context, h *jobs.Handle, checkpoint string) *jobs.Outcome {
	if err := h.Checkpoint(ctx, checkpoint); err != nil {
		return failed(modInstallFailed(apierr.Internal, err))
	}
	return nil
}

// serverDir is 02 §3's server/ under an instance's data directory — the only tree a mod
// install writes to.
func serverDir(inst *store.Instance) string { return filepath.Join(inst.DataDir, "server") }

// resolveForInstall computes the closure and drops the nodes that need no work. A nil
// outcome means the packages returned are the ones to install; a non-nil one is the
// terminal answer.
func (m *Mods) resolveForInstall(
	ctx context.Context, instanceID string, payload modInstallPayload,
) ([]*stagedPackage, *jobs.Outcome) {
	idx := &storeIndex{ctx: ctx, db: m.DB, instanceID: instanceID}
	closure, resolveErr := modresolver.Resolve(
		[]modresolver.Request{{FullName: payload.FullName, Version: payload.Version}}, idx)
	if idx.err != nil {
		return nil, failed(modInstallFailed(apierr.Internal, idx.err))
	}
	if resolveErr != nil {
		return nil, failed(modInstallFailed(apierr.DependencyUnresolved, resolveErr))
	}

	installed, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, failed(modInstallFailed(apierr.Internal, err))
	}
	have := make(map[string]string, len(installed))
	for i := range installed {
		have[installed[i].FullName] = installed[i].Version
	}

	var out []*stagedPackage
	for _, n := range closure.Nodes {
		if n.NoOp {
			continue
		}
		// `↯` Changing an installed package's version is an *update*, which WP-M2-09 owns:
		// it is uninstall-then-install in one job, because the old version's files are only
		// removable from the old version's manifest. Installing over it here would leave
		// every file the new version does not also ship as an orphan that BepInEx would
		// happily load. Refused rather than half-done.
		if current, ok := have[n.FullName]; ok && current != n.Version {
			return nil, failed(jobs.Outcome{
				Status: "failed", ErrorCode: apierr.ModConflict.String(),
				Error: fmt.Sprintf("%s is installed at %s; changing it to %s is an update, "+
					"which is a separate operation", n.FullName, current, n.Version),
			})
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
		out = append(out, &stagedPackage{fullName: n.FullName, version: n.Version, transitive: n.Transitive})
	}
	return out, nil
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
		for _, e := range manifest {
			claims[e.Path] = p.fullName
		}
	}
	return nil
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
	var stuck []string
	for _, p := range pkgs {
		if err := installer.Rollback(p.manifest, serverRoot, backupDir); err != nil {
			slog.ErrorContext(ctx, "mod install rollback incomplete",
				slog.String("instance_id", inst.ID), slog.String("full_name", p.fullName),
				slog.Any("error", err))
			stuck = append(stuck, p.fullName)
			continue
		}
		rolled = append(rolled, p.fullName)
	}
	if err := m.DB.DeleteInstanceMods(ctx, inst.ID, rolled); err != nil {
		return modInstallFailed(apierr.Internal,
			fmt.Errorf("%w; and removing its rows failed: %w", cause, err))
	}
	if len(stuck) > 0 {
		// An install that failed is ordinary; one that could not be undone is not, and the
		// operator has to be told which packages still have files on disk.
		return modInstallFailed(apierr.Internal,
			fmt.Errorf("%w; and these could not be rolled back: %s", cause, strings.Join(stuck, ", ")))
	}
	return modInstallFailed(apierr.Internal, cause)
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
	return modInstallFailed(apierr.Internal, err)
}

func modInstallFailed(code apierr.Code, err error) jobs.Outcome {
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
