package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

type resolveRequest struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	// Source is the registry the operator picked the package from. It is optional: a body
	// that omits it resolves from whichever registry carries the version, preferring none.
	Source string `json:"source"`
}

type resolvedNode struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	// FromVersion is the installed version, empty when the package is not installed.
	FromVersion string `json:"from_version"`
	Version     string `json:"version"`
	// Change is none, install, upgrade or downgrade.
	Change     string `json:"change"`
	Transitive bool   `json:"transitive"`
	NoOp       bool   `json:"no_op"`
}

type resolveResponse struct {
	Nodes []resolvedNode `json:"nodes"`
	// Removals are installed packages the change uninstalls: members a modpack's new version drops.
	Removals []removalView `json:"removals"`
	// Kept are modpack members the change leaves at a version other than the pack's, and why.
	Kept []keptMember `json:"kept"`
	// Conflicts are dependencies the change would leave unmet. The install refuses while any remain.
	Conflicts []conflictView `json:"conflicts"`
	// Backup reports whether the install archives the world first: it replaces or removes an
	// installed version on a server that has a world.
	Backup bool `json:"backup"`
}

// resolve is POST /instances/{id}/mods/resolve (04 §3): a dry run over the cached index, no
// download or write, gated on mods.manage since it previews what an install would do.
func (m *Mods) resolve(w http.ResponseWriter, r *http.Request) {
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

	body, ok := decodePackageRequest(w, r)
	if !ok {
		return
	}

	// The same plan the install would compute, framework auto-install included: a preview
	// omitting the BepInEx a vanilla instance is about to gain would show the wrong thing.
	prefer, _ := source.ByName(body.Source)
	idx := m.newStoreIndex(r.Context(), id, prefer)
	plan, resolveErr := m.planInstall(r.Context(), inst, body.FullName, body.Version, idx)
	// idx.err, not resolveErr, is checked first: a genuine read failure must never be
	// reported as dependency_unresolved just because Dependencies degraded to (nil,
	// false) to satisfy modresolver.Index's error-free signature.
	if idx.err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(idx.err))
		return
	}
	if resolveErr != nil {
		writeResolveError(w, r, resolveErr)
		return
	}
	if off := disabledInClosure(closureNames(plan.closure), idx.rows()); len(off) > 0 {
		writeDisabledConflict(w, r, off)
		return
	}

	resp := planResponse(&plan, idx)
	resp.Backup = resp.Backup && hasWorlds(inst)
	JSON(w, r, http.StatusOK, resp)
}

// planResponse turns a plan into the preview. Backup is set when any node moves an installed
// package to another version or the plan removes one; the caller clears it for a server with no
// world.
func planResponse(plan *changePlan, idx *storeIndex) resolveResponse {
	resp := resolveResponse{
		Nodes:     make([]resolvedNode, 0, len(plan.closure.Nodes)),
		Removals:  removalViews(plan.removals, idx),
		Kept:      append([]keptMember{}, plan.kept...),
		Conflicts: toConflictViews(plan.conflicts, idx),
		Backup:    len(plan.removals) > 0,
	}
	for _, n := range plan.closure.Nodes {
		from := idx.have[n.FullName].Version
		resp.Nodes = append(resp.Nodes, resolvedNode{
			FullName: n.FullName, Source: idx.sourceOf(n.FullName, n.Version).String(),
			FromVersion: from, Version: n.Version, Change: changeOf(from, n.Version, n.NoOp),
			Transitive: n.Transitive, NoOp: n.NoOp,
		})
		if !n.NoOp && from != "" && from != n.Version {
			resp.Backup = true
		}
	}
	return resp
}

// writeResolveError maps the resolver's typed failures onto 11 §2.5's dependency_unresolved:
// from the caller's side a cycle, a malformed ident and an unusable version are one answer,
// this closure cannot be computed. A request to move a locked package is mod_conflict, naming
// it in details.locked. `details.missing` names whatever could not be resolved. The
// 500 below is reserved for a genuine panel fault, the index being externally sourced.
func writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	unresolvable := func(missing string) {
		apierr.Write(w, r, apierr.New(apierr.DependencyUnresolved).With("missing", missing))
	}

	var unresolved *modresolver.UnresolvedError
	if errors.As(err, &unresolved) {
		unresolvable(unresolved.Ident())
		return
	}
	var malformed *modresolver.MalformedDependencyError
	if errors.As(err, &malformed) {
		unresolvable(malformed.Ident())
		return
	}
	var held *modresolver.HeldError
	if errors.As(err, &held) {
		apierr.Write(w, r, apierr.New(apierr.ModConflict).With("locked", held.FullName).Wrap(err))
		return
	}
	var badVersion *modresolver.BadVersionError
	if errors.As(err, &badVersion) {
		unresolvable(badVersion.FullName + "-" + badVersion.Version)
		return
	}
	var cycle *modresolver.CycleError
	if errors.As(err, &cycle) {
		unresolvable(strings.Join(cycle.Cycle, " -> "))
		return
	}
	apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
}

// storeIndex adapts the store to modresolver.Index, which stays pure and never imports store
// (CLAUDE.md §5). Its methods have no room to return an infrastructure error, so storeIndex
// captures the first one it sees and the caller checks idx.err after Resolve returns.
//
// It is also where a package's registry is decided. A dependency ident cannot name one
// (03 §6.2), so the resolver stays registry-blind and this adapter records, per package, which
// registry actually answered — installed first, then the one the operator picked, then
// whichever carries the version (B14).
type storeIndex struct {
	ctx context.Context
	db  *store.DB
	// prefer is the registry the request named, used wherever more than one carries a version.
	prefer  source.Source
	enabled []source.Source
	// chosen is the registry each package-version resolved from, read back after Resolve
	// returns. Keyed by version and not by package: the resolver expands every requested
	// version of a package and keeps the highest, so a map keyed by name alone would end up
	// naming whichever registry answered last, which need not be the one carrying the version
	// that won. That mismatch downloads from a registry the version is not on (B14).
	chosen map[string]source.Source
	// have is the instance's installed packages, read once. An installed package's registry
	// outranks every other consideration for that package.
	have map[string]store.CataloguedMod
	// held is the packages no request or edge may move: the locked ones, and whatever a
	// modpack plan keeps as a local override.
	held map[string]bool
	// requested is the package an install names. When the request names a registry too, that
	// package comes from that registry and no other.
	requested string
	err       error
}

// newStoreIndex reads the instance's installed packages once. A read failure is kept in idx.err,
// which every caller checks after resolving.
func (m *Mods) newStoreIndex(
	ctx context.Context, instanceID string, prefer source.Source,
) *storeIndex {
	idx := &storeIndex{
		ctx: ctx, db: m.DB,
		enabled: m.enabledSources(),
		prefer:  prefer,
		chosen:  map[string]source.Source{},
		have:    map[string]store.CataloguedMod{},
		held:    map[string]bool{},
	}
	rows, err := m.DB.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		idx.err = err
		return idx
	}
	for i := range rows {
		idx.have[rows[i].FullName] = rows[i]
		idx.held[rows[i].FullName] = rows[i].Locked
	}
	return idx
}

// rows is the installed packages as plain rows, ordered by full name.
func (idx *storeIndex) rows() []store.InstanceMod {
	out := make([]store.InstanceMod, 0, len(idx.have))
	for _, name := range slices.Sorted(maps.Keys(idx.have)) {
		out = append(out, idx.have[name].InstanceMod)
	}
	return out
}

// versionKey is chosen's key: a package at one exact version.
func versionKey(fullName, version string) string { return fullName + "@" + version }

// sourceOf reports the registry supplying a resolved or already-installed version.
func (idx *storeIndex) sourceOf(fullName, version string) source.Source {
	if row, ok := idx.have[fullName]; ok && row.Version == version {
		return row.Source
	}
	return idx.chosen[versionKey(fullName, version)]
}

func (idx *storeIndex) Dependencies(fullName, version string) ([]string, bool) {
	if idx.err != nil {
		return nil, false
	}
	allowed := idx.enabled
	prefer := idx.prefer
	if prefer == (source.Source{}) && len(allowed) > 0 {
		prefer = allowed[0]
	}
	// Existing packages must retain their registry, even when another has a newer version. The
	// installed version's own edges are read even from a registry that is switched off; moving
	// to another version needs it enabled.
	if row, ok := idx.have[fullName]; ok {
		if row.Version != version && !slices.Contains(allowed, row.Source) {
			return nil, false
		}
		allowed = []source.Source{row.Source}
	}
	if fullName == idx.requested && idx.prefer != (source.Source{}) {
		if !slices.Contains(allowed, idx.prefer) {
			return nil, false
		}
		allowed = []source.Source{idx.prefer}
	}
	deps, foundIn, ok, err := idx.db.ModVersionDependenciesFrom(idx.ctx, fullName, version, prefer, allowed)
	if err != nil {
		idx.err = err
		return nil, false
	}
	if ok {
		idx.chosen[versionKey(fullName, version)] = foundIn
	}
	return deps, ok
}

func (idx *storeIndex) Installed(fullName string) (string, bool) {
	row, ok := idx.have[fullName]
	return row.Version, ok
}

func (idx *storeIndex) Held(fullName string) bool { return idx.held[fullName] }
