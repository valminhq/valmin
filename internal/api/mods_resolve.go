package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

type resolveRequest struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
}

type resolvedNode struct {
	FullName   string `json:"full_name"`
	Version    string `json:"version"`
	Transitive bool   `json:"transitive"`
	NoOp       bool   `json:"no_op"`
}

type resolveResponse struct {
	Nodes []resolvedNode `json:"nodes"`
}

// resolve is POST /instances/{id}/mods/resolve (04 §3): a dry run over the cached index —
// no download, no write, gated on mods.manage since it previews what an install would do.
// `04 §3`'s own comment on the route is the reason it exists at all: the user confirms
// the closure before anything is downloaded or written.
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

	idx := &storeIndex{ctx: r.Context(), db: m.DB, instanceID: id}
	closure, resolveErr := modresolver.Resolve(
		[]modresolver.Request{{FullName: body.FullName, Version: body.Version}}, idx,
	)
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

	nodes := make([]resolvedNode, 0, len(closure.Nodes))
	for _, n := range closure.Nodes {
		nodes = append(nodes, resolvedNode{
			FullName: n.FullName, Version: n.Version, Transitive: n.Transitive, NoOp: n.NoOp,
		})
	}
	JSON(w, r, http.StatusOK, resolveResponse{Nodes: nodes})
}

// writeResolveError maps the resolver's typed failures onto 11 §2.5's
// dependency_unresolved. The registry has no separate code for a cycle, a malformed
// dependency ident or an unusable version — the M2 plan's Decision 7 added exactly two
// codes, neither of them these — and from the caller's side all four are one answer: this
// closure cannot be computed from the index as it stands. `details.missing` names
// whatever could not be resolved. Only a genuine panel fault reaches the 500 below; the
// index is externally sourced, so dirty data in it is never one.
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

// storeIndex adapts the store to modresolver.Index. It captures the first infrastructure
// error it sees rather than returning one from Dependencies/Installed — modresolver.Index's
// signature has no room for one, by design (CLAUDE.md §5: resolver stays pure and never
// imports store) — so the caller checks idx.err after Resolve returns, before trusting
// any verdict Resolve gave.
type storeIndex struct {
	ctx        context.Context
	db         *store.DB
	instanceID string
	err        error
}

func (idx *storeIndex) Dependencies(fullName, version string) ([]string, bool) {
	if idx.err != nil {
		return nil, false
	}
	deps, ok, err := idx.db.ModVersionDependencies(idx.ctx, fullName, version)
	if err != nil {
		idx.err = err
		return nil, false
	}
	return deps, ok
}

func (idx *storeIndex) Installed(fullName string) (string, bool) {
	if idx.err != nil {
		return "", false
	}
	version, ok, err := idx.db.InstanceModVersion(idx.ctx, idx.instanceID, fullName)
	if err != nil {
		idx.err = err
		return "", false
	}
	return version, ok
}
