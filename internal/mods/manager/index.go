package manager

import (
	"context"
	"maps"
	"slices"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// Index adapts the store to modresolver.Index, which stays pure and never imports store
// (CLAUDE.md §5). Its methods have no room to return an infrastructure error, so Index
// captures the first one it sees and the caller checks idx.Err after Resolve returns.
//
// It is also where a package's registry is decided. A dependency ident cannot name one
// (03 §6.2), so the resolver stays registry-blind and this adapter records, per package, which
// registry actually answered — installed first, then the one the operator picked, then
// whichever carries the version (B14).
type Index struct {
	ctx context.Context
	db  *store.DB
	// prefer is the registry the request named, used wherever more than one carries a version.
	Prefer  source.Source
	enabled []source.Source
	// chosen is the registry each package-version resolved from, read back after Resolve
	// returns. Keyed by version and not by package: the resolver expands every requested
	// version of a package and keeps the highest, so a map keyed by name alone would end up
	// naming whichever registry answered last, which need not be the one carrying the version
	// that won. That mismatch downloads from a registry the version is not on (B14).
	chosen map[string]source.Source
	// have is the instance's installed packages, read once. An installed package's registry
	// outranks every other consideration for that package.
	Have map[string]store.CataloguedMod
	// held is the packages no request or edge may move: the locked ones, and whatever a
	// modpack plan keeps as a local override.
	HeldRows map[string]bool
	// requested is the package an install names. When the request names a registry too, that
	// package comes from that registry and no other.
	Requested string
	Err       error
}

// NewIndex reads the instance's installed packages once. A read failure is kept in idx.Err,
// which every caller checks after resolving.
func NewIndex(
	ctx context.Context, db *store.DB, instanceID string, prefer source.Source, enabled []source.Source,
) *Index {
	idx := &Index{
		ctx: ctx, db: db,
		enabled:  enabled,
		Prefer:   prefer,
		chosen:   map[string]source.Source{},
		Have:     map[string]store.CataloguedMod{},
		HeldRows: map[string]bool{},
	}
	rows, err := db.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		idx.Err = err
		return idx
	}
	for i := range rows {
		idx.Have[rows[i].FullName] = rows[i]
		idx.HeldRows[rows[i].FullName] = rows[i].Locked
	}
	return idx
}

// rows is the installed packages as plain rows, ordered by full name.
func (idx *Index) Rows() []store.InstanceMod {
	out := make([]store.InstanceMod, 0, len(idx.Have))
	for _, name := range slices.Sorted(maps.Keys(idx.Have)) {
		out = append(out, idx.Have[name].InstanceMod)
	}
	return out
}

// versionKey is chosen's key: a package at one exact version.
func versionKey(fullName, version string) string { return fullName + "@" + version }

// SourceOf reports the registry supplying a resolved or already-installed version.
func (idx *Index) SourceOf(fullName, version string) source.Source {
	if row, ok := idx.Have[fullName]; ok && row.Version == version {
		return row.Source
	}
	return idx.chosen[versionKey(fullName, version)]
}

func (idx *Index) Dependencies(fullName, version string) ([]string, bool) {
	if idx.Err != nil {
		return nil, false
	}
	allowed := idx.enabled
	prefer := idx.Prefer
	if prefer == (source.Source{}) && len(allowed) > 0 {
		prefer = allowed[0]
	}
	// Existing packages must retain their registry, even when another has a newer version. The
	// installed version's own edges are read even from a registry that is switched off; moving
	// to another version needs it enabled.
	if row, ok := idx.Have[fullName]; ok {
		if row.Version != version && !slices.Contains(allowed, row.Source) {
			return nil, false
		}
		allowed = []source.Source{row.Source}
	}
	if fullName == idx.Requested && idx.Prefer != (source.Source{}) {
		if !slices.Contains(allowed, idx.Prefer) {
			return nil, false
		}
		allowed = []source.Source{idx.Prefer}
	}
	deps, foundIn, ok, err := idx.db.ModVersionDependenciesFrom(idx.ctx, fullName, version, prefer, allowed)
	if err != nil {
		idx.Err = err
		return nil, false
	}
	if ok {
		idx.chosen[versionKey(fullName, version)] = foundIn
	}
	return deps, ok
}

func (idx *Index) Installed(fullName string) (string, bool) {
	row, ok := idx.Have[fullName]
	return row.Version, ok
}

func (idx *Index) Held(fullName string) bool { return idx.HeldRows[fullName] }
