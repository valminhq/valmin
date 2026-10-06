package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/mods/manager"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
)

type resolveRequest struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	// Source is the registry the operator picked the package from. It is optional: a body
	// that omits it resolves from whichever registry carries the version, preferring none.
	Source string `json:"source"`
}

func domainPackage(req resolveRequest) manager.PackageRequest {
	return manager.PackageRequest{FullName: req.FullName, Version: req.Version, Source: req.Source}
}

func domainPackages(requests []resolveRequest) []manager.PackageRequest {
	packages := make([]manager.PackageRequest, len(requests))
	for i, req := range requests {
		packages[i] = domainPackage(req)
	}
	return packages
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
	Kept []manager.KeptMember `json:"kept"`
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
	plan, resolveErr := m.plan.PlanInstall(r.Context(), inst, body.FullName, body.Version, idx)
	// idx.Err(), not resolveErr, is checked first: a genuine read failure must never be
	// reported as dependency_unresolved just because Dependencies degraded to (nil,
	// false) to satisfy modresolver.Index's error-free signature.
	if idx.Err() != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(idx.Err()))
		return
	}
	if resolveErr != nil {
		writeResolveError(w, r, resolveErr)
		return
	}
	if off := manager.DisabledInClosure(manager.ClosureNames(plan.Closure), idx.Rows()); len(off) > 0 {
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
func planResponse(plan *manager.ChangePlan, idx *manager.Index) resolveResponse {
	resp := resolveResponse{
		Nodes:     make([]resolvedNode, 0, len(plan.Closure.Nodes)),
		Removals:  removalViews(plan.Removals, idx),
		Kept:      append([]manager.KeptMember{}, plan.Kept...),
		Conflicts: toConflictViews(plan.Conflicts, idx),
		Backup:    len(plan.Removals) > 0,
	}
	for _, n := range plan.Closure.Nodes {
		from, _ := idx.Installed(n.FullName)
		resp.Nodes = append(resp.Nodes, resolvedNode{
			FullName: n.FullName, Source: idx.SourceOf(n.FullName, n.Version).String(),
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
		apierr.Write(w, r, apierr.New(errcode.DependencyUnresolved).With("missing", missing))
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
		apierr.Write(w, r, apierr.New(errcode.ModConflict).With("locked", held.FullName).Wrap(err))
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
	apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
}

func (m *Mods) newStoreIndex(ctx context.Context, instanceID string, prefer source.Source) *manager.Index {
	return manager.NewIndex(ctx, m.DB, instanceID, prefer, m.enabledSources())
}
