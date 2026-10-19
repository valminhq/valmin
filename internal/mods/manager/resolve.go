package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// prepareInstall is everything that can still be abandoned: resolve, download, unpack, and
// work out what would change. Nothing it does is visible in server/ or in the database, so
// a failure or a cancellation here needs no undoing beyond deleting the staging directory.
func (i *Installer) prepareInstall(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, payload *InstallPayload,
) ([]*stagedPackage, error) {
	h.Progress(ctx, 5, "resolving dependencies")
	pkgs, err := i.ResolveForInstall(ctx, inst, payload)
	if err != nil {
		return nil, err
	}
	if err := mark(ctx, h, CheckpointResolved); err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, nil
	}

	h.Progress(ctx, 20, fmt.Sprintf("downloading %d packages", len(pkgs)))
	if err := i.downloadClosure(ctx, h, pkgs); err != nil {
		if errors.Is(err, errDownloadCancelled) {
			return nil, err
		}
		return nil, coded(errcode.Unavailable, err)
	}
	if err := mark(ctx, h, CheckpointDownloaded); err != nil {
		return nil, err
	}

	h.Progress(ctx, 45, "unpacking")
	if err := stageClosure(pkgs, payload.StagingDir); err != nil {
		return nil, coded(errcode.PackageInvalid, err)
	}
	if err := mark(ctx, h, CheckpointStaged); err != nil {
		return nil, err
	}

	h.Progress(ctx, 60, "checking what would change")
	if err := i.planClosure(ctx, inst.ID, instance.ServerDir(inst.DataDir), pkgs, payload.Unowned); err != nil {
		return nil, planError(err)
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

// ResolveForInstall computes the closure and drops the nodes that need no work, returning the
// packages to install.
func (i *Installer) ResolveForInstall(
	ctx context.Context, inst *store.Instance, payload *InstallPayload,
) ([]*stagedPackage, error) {
	plan, idx, err := i.jobPlan(ctx, inst, payload)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		out = append(out, p)
	}
	for _, n := range plan.Closure.Nodes {
		if n.NoOp {
			continue
		}
		p, err := stageNode(n, idx.SourceOf(n.FullName, n.Version), have[n.FullName], len(payload.Updates) > 0)
		if err != nil {
			return nil, err
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
) (ChangePlan, *Index, error) {
	prefer, _ := source.ByName(payload.Source)
	idx := i.newIndex(ctx, inst.ID, prefer)
	plan, err := i.planPayload(ctx, inst, payload, idx)
	switch {
	case idx.err != nil:
		return plan, idx, idx.err
	case err != nil:
		return plan, idx, coded(resolveFailure(err), err)
	case len(plan.Conflicts) > 0:
		return plan, idx, coded(errcode.ModConflict,
			fmt.Errorf("the change would break a dependency: %s", describeConflicts(plan.Conflicts)))
	}
	if off := DisabledInClosure(ClosureNames(plan.Closure), idx.Rows()); len(off) > 0 {
		return plan, idx, coded(errcode.ModConflict,
			fmt.Errorf("these mods are disabled; enable them first: %s", strings.Join(off, ", ")))
	}
	return plan, idx, nil
}

// stageNode turns one resolved node into the package the job places. current is the installed
// row of that package, nil when it is not installed; update is true for "Update all".
func stageNode(
	n modresolver.Node, src source.Source, current *store.InstanceMod, update bool,
) (*stagedPackage, error) {
	// Checked here, ahead of Plan's own check: the job stages each package into a
	// directory named after it, so a full name from the index reaches the filesystem
	// here first. A name containing `..` would extract outside the staging root (B5).
	if err := installer.CheckFullName(n.FullName); err != nil {
		return nil, coded(errcode.PackageInvalid, err)
	}
	// A package whose registry never resolved would download from nowhere and be recorded
	// as coming from nowhere. Failing here keeps that from reaching disk (B14).
	if src == (source.Source{}) {
		return nil, fmt.Errorf("%s-%s resolved without a registry", n.FullName, n.Version)
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
		return nil, err
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
	return i.Planner().PlanInstall(ctx, inst, payload.FullName, version, idx)
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
// errDownloadCancelled is a download phase stopped by a cancel request between packages.
var errDownloadCancelled = errors.New("download cancelled")

// downloadClosure fetches every package's zip, checking for a cancel request before each one.
func (i *Installer) downloadClosure(ctx context.Context, h *jobs.Handle, pkgs []*stagedPackage) error {
	for _, p := range pkgs {
		if p.remove {
			continue
		}
		if h.CancelRequested(ctx) {
			return errDownloadCancelled
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
// closure, so two packages colliding on one path is caught here rather than at write time. A path
// a package in the closure removes or updates is open to any of them, so a file can move from one
// package to another in one change.
func (i *Installer) planClosure(
	ctx context.Context, instanceID, serverRoot string, pkgs []*stagedPackage, unowned installer.Unowned,
) error {
	claims, err := i.installedClaims(ctx, instanceID)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		for _, e := range p.prevManifest {
			claims[e.Path] = ""
		}
	}
	var waiting []string
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
		waiting = append(waiting, installer.ApplyUnowned(changes, unowned)...)
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
	if len(waiting) > 0 {
		return &installer.UnownedError{Paths: waiting}
	}
	setStale(pkgs, claims)
	return nil
}

// setStale records what each package removes: its stale paths, less any that another package
// in the closure now claims.
func setStale(pkgs []*stagedPackage, claims map[string]string) {
	for _, p := range pkgs {
		if !p.remove {
			p.prevStale = staleOf(p)
		}
		p.prevStale = slices.DeleteFunc(p.prevStale, func(path string) bool {
			return claims[path] != "" && claims[path] != p.fullName
		})
	}
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
