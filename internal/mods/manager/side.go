package manager

import (
	"context"
	"fmt"
	"sort"

	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

// sideRank orders the four tags by how much a client needs the package, so that a dependency
// reached from two parents takes the stronger claim: a package one mod merely offers to a
// client and another requires is required (ADR-175). Untagged is weakest, and server-only
// outranks it because it is a statement and not an absence.
var sideRank = map[string]int{
	store.SideUnknown: 0,
	"server_only":     1,
	"client_optional": 2,
	"client_required": 3,
}

// weaker reports whether the tag a mod carries claims less than side.
func weaker(current, side string) bool { return sideRank[current] < sideRank[side] }

// dependenciesToRaise walks fullName's transitive closure at the versions this instance has
// installed and returns the packages whose tag claims less than side. Packages that are not
// installed are not in the closure: a tag is a row on an installed mod, and the export is
// where a missing dependency is reported (04 §3).
//
// A version the index cannot describe ends that branch. The dependencies of a package the
// index has never seen are unknown rather than empty, and a tag edit is not the place to
// refuse over it — the export already reports that closure as a conflict.
func (p *Planner) DependenciesToRaise(
	ctx context.Context, installed []store.InstanceMod, fullName, side string,
) ([]string, error) {
	byName := make(map[string]*store.InstanceMod, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = &installed[i]
	}

	raise := []string{}
	seen := map[string]bool{fullName: true}
	for queue := []string{fullName}; len(queue) > 0; {
		parent := queue[0]
		queue = queue[1:]
		mod, ok := byName[parent]
		if !ok {
			continue
		}
		deps, _, ok, err := p.DB.ModVersionDependencies(ctx, parent, mod.Version, mod.Source)
		if err != nil {
			return nil, fmt.Errorf("read dependencies of %s: %w", parent, err)
		}
		if !ok {
			continue
		}
		for _, dep := range deps {
			name, _, ok := modresolver.ParseDependency(dep)
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			queue = append(queue, name)
			if installedDep, ok := byName[name]; ok && weaker(installedDep.Side, side) {
				raise = append(raise, name)
			}
		}
	}
	sort.Strings(raise)
	return raise, nil
}
