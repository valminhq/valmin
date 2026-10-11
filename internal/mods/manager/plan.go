package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

type Planner struct {
	DB      *store.DB
	Enabled []source.Source
}

// modpackCategory is the catalogue category Thunderstore-compatible registries file a modpack
// under: a package whose dependencies are the mods it bundles, each pinned to one version.
const modpackCategory = "Modpacks"

// Why a modpack member stays at a version other than the pack's.
const (
	keptLocked   = "locked"
	keptManual   = "manual"
	keptNewer    = "newer"
	keptChanged  = "changed"
	keptDisabled = "disabled"
	keptRequired = "required"
)

// ChangePlan is everything one install or update would do: the closure the resolver computes,
// the installed packages it removes, the modpack members it leaves where they are, and the
// dependencies the result would leave unmet. A plan with conflicts is never applied.
type ChangePlan struct {
	Closure   modresolver.Closure
	Removals  []string
	Kept      []KeptMember
	Conflicts []modresolver.Conflict
}

// KeptMember is a modpack member a change leaves at a version other than the pack's.
type KeptMember struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	// PackVersion is the version the pack pins, empty for a member the new pack version drops.
	PackVersion string `json:"pack_version"`
	Reason      string `json:"reason"`
}

// packChange is how a modpack install or version change treats the pack's members. IsPack is
// false, and the rest empty, for a package that is not a modpack.
type packChange struct {
	IsPack   bool
	requests []modresolver.Request
	kept     []KeptMember
	// dropped maps each member the installed pack version pins and the new one does not to the
	// version the installed one pins.
	dropped map[string]string
}

// IsPack reports whether a catalogue row is filed as a modpack.
func IsPack(pkg *store.ModPackage) bool {
	if pkg == nil {
		return false
	}
	var categories []string
	_ = json.Unmarshal([]byte(pkg.CategoriesJSON), &categories)
	return slices.Contains(categories, modpackCategory)
}

// pins maps a package's dependency idents to the version each names.
func pins(deps []string) map[string]string {
	out := make(map[string]string, len(deps))
	for _, dep := range deps {
		if name, version, ok := modresolver.ParseDependency(dep); ok {
			out[name] = version
		}
	}
	return out
}

// FollowsPack reports whether an installed member is still the pack's own: installed with it,
// unlocked, and at the version the pack pins. Any other member is a local override.
func FollowsPack(row *store.InstanceMod, pin string) bool {
	return row.InstalledAs == store.InstalledDependency && !row.Locked && row.Version == pin
}

// planPack applies a modpack version to its members. A member that follows the installed pack
// version moves to the new pin, up or down. A locked or manually installed member keeps its
// version and is held there. Any other installed member is treated as a plain dependency, raised
// to the pin when it is older and kept when it is newer. BepInEx is left to the install's own
// framework rule.
func (p *Planner) planPack(
	ctx context.Context, fullName, version string, idx *Index,
) (packChange, error) {
	change := packChange{dropped: map[string]string{}}
	pkg, err := IndexedPackage(ctx, p.DB, fullName, idx.prefer, p.Enabled)
	if err != nil {
		return change, err
	}
	if change.IsPack = IsPack(pkg); !change.IsPack {
		return change, nil
	}
	deps, ok := idx.Dependencies(fullName, version)
	if !ok {
		return change, nil
	}
	next := pins(deps)
	prev := map[string]string{}
	if row, installed := idx.have[fullName]; installed {
		if deps, ok := idx.Dependencies(fullName, row.Version); ok {
			prev = pins(deps)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(next)) {
		change.member(name, next[name], prev[name], idx)
	}
	for name, pin := range prev {
		if _, still := next[name]; !still && name != BepInExPack {
			change.dropped[name] = pin
		}
	}
	return change, nil
}

// member decides one member the new pack version pins at pin, prevPin being the installed pack
// version's pin, empty when it named none.
func (c *packChange) member(name, pin, prevPin string, idx *Index) {
	row, installed := idx.have[name]
	if !installed || name == BepInExPack {
		return
	}
	kept := KeptMember{FullName: name, Version: row.Version, PackVersion: pin}
	switch {
	case row.Locked, row.InstalledAs == store.InstalledExplicit:
		idx.held[name] = true
		kept.Reason = keptManual
		if row.Locked {
			kept.Reason = keptLocked
		}
		if row.Version != pin {
			c.kept = append(c.kept, kept)
		}
	case prevPin == row.Version:
		c.requests = append(c.requests, modresolver.Request{FullName: name, Version: pin})
	case newer(row.Version, pin):
		kept.Reason = keptNewer
		c.kept = append(c.kept, kept)
	}
}

// PlanInstall is what installing fullName at version would do, and what the resolve dry run
// previews. One function, because the dry run exists so the user confirms the change before
// anything downloads: two code paths would drift, and the preview is the one nobody notices.
func (p *Planner) PlanInstall(
	ctx context.Context, inst *store.Instance, fullName, version string, idx *Index,
) (ChangePlan, error) {
	if idx.prefer != (source.Source{}) {
		if !slices.Contains(p.Enabled, idx.prefer) {
			return ChangePlan{}, &modresolver.UnresolvedError{FullName: fullName, Version: version}
		}
	}
	idx.requested = fullName
	pack, err := p.planPack(ctx, fullName, version, idx)
	if err != nil {
		return ChangePlan{}, err
	}
	requests := append([]modresolver.Request{{FullName: fullName, Version: version}}, pack.requests...)
	if fullName != BepInExPack {
		framework, ok, err := p.frameworkVersion(ctx, idx)
		if err != nil {
			return ChangePlan{}, err
		}
		if ok {
			requests = append(requests, modresolver.Request{FullName: BepInExPack, Version: framework})
		}
	}
	closure, err := modresolver.Resolve(requests, idx)
	if idx.err != nil {
		// A store read failed, so the verdict is worthless. The caller checks idx.err first;
		// reporting the resolver's error here would surface a database fault to the user as
		// dependency_unresolved.
		return ChangePlan{}, nil //nolint:nilerr // idx.err is the real failure, and the caller reads it
	}
	if err != nil {
		return ChangePlan{}, fmt.Errorf("resolve %s-%s: %w", fullName, version, err)
	}
	if !inst.Modded && !hasNode(closure, BepInExPack) {
		return ChangePlan{}, &modresolver.UnresolvedError{FullName: BepInExPack, Version: "latest"}
	}

	plan := ChangePlan{Closure: markTransitive(closure, fullName), Kept: pack.kept}
	for _, n := range plan.Closure.Nodes {
		if !n.NoOp {
			// Something in the closure installs or moves it, so it is not the pack's to drop.
			delete(pack.dropped, n.FullName)
		}
	}
	removals, kept := dropPlan(pack.dropped, plannedVersions(plan.Closure, idx, nil), idx)
	plan.Removals, plan.Kept = removals, append(plan.Kept, kept...)
	packs := installedPacks(idx)
	packs[fullName] = pack.IsPack
	plan.Conflicts = conflictsOf(plan.Closure, plan.Removals, packs, idx)
	return plan, idx.err
}

// frameworkVersion is the BepInEx version an install asks for. BepInEx older than the game build
// crashes the server on boot, so it is the newest version, unless the installed one is locked or
// already newer. ok is false when no registry lists BepInEx and none is installed.
func (p *Planner) frameworkVersion(ctx context.Context, idx *Index) (version string, ok bool, err error) {
	latest, ok, err := LatestBepInEx(ctx, p.DB, p.Enabled, idx.prefer)
	if err != nil {
		return "", false, err
	}
	installed, present := idx.Installed(BepInExPack)
	if present && (idx.Held(BepInExPack) || newer(installed, latest)) {
		return installed, true, nil
	}
	return latest, ok, nil
}

// PlanUpdates is "Update all"'s plan: every target resolved at once, so a dependency two updates
// share is raised once to the higher of their demands, not installed twice in two jobs.
func PlanUpdates(targets []UpdateTarget, idx *Index) (ChangePlan, error) {
	requests := make([]modresolver.Request, 0, len(targets))
	for _, t := range targets {
		requests = append(requests, modresolver.Request{FullName: t.FullName, Version: t.Version})
	}
	closure, err := modresolver.Resolve(requests, idx)
	if idx.err != nil {
		// As in PlanInstall: a store read failed, so the verdict is worthless and the caller
		// reports idx.err instead.
		return ChangePlan{}, nil //nolint:nilerr // idx.err is the real failure, and the caller reads it
	}
	if err != nil {
		return ChangePlan{}, fmt.Errorf("resolve %d updates: %w", len(targets), err)
	}
	plan := ChangePlan{Closure: closure}
	plan.Conflicts = conflictsOf(closure, nil, installedPacks(idx), idx)
	return plan, idx.err
}

// dropPlan decides the members a new modpack version no longer pins. One still at the old pin,
// installed as a dependency, and needed by nothing that remains is removed. Any other is kept and
// reported with the reason.
func dropPlan(
	dropped, planned map[string]string, idx *Index,
) (removals []string, kept []KeptMember) {
	remove := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(dropped)) {
		row, ok := idx.have[name]
		if !ok {
			continue
		}
		if reason := dropRefusal(&row.InstanceMod, dropped[name]); reason != "" {
			kept = append(kept, KeptMember{FullName: name, Version: row.Version, Reason: reason})
			continue
		}
		remove[name] = true
	}
	if len(remove) == 0 {
		return nil, kept
	}
	kept = append(kept, keepRequired(remove, planned, idx)...)
	return slices.Sorted(maps.Keys(remove)), kept
}

// dropRefusal is why a member the pack drops stays installed, or "" when it can go.
func dropRefusal(row *store.InstanceMod, pin string) string {
	switch {
	case row.Locked:
		return keptLocked
	case row.InstalledAs == store.InstalledExplicit:
		return keptManual
	case !row.Enabled:
		return keptDisabled
	case row.Version != pin:
		return keptChanged
	}
	return ""
}

// keepRequired takes out of remove every package something that stays still depends on, to a
// fixed point, since keeping one keeps whatever it needs.
func keepRequired(remove map[string]bool, planned map[string]string, idx *Index) []KeptMember {
	needs := map[string]map[string]string{}
	for name, version := range planned {
		deps, _ := idx.Dependencies(name, version)
		needs[name] = pins(deps)
	}
	var kept []KeptMember
	for again := true; again; {
		again = false
		for _, name := range slices.Sorted(maps.Keys(remove)) {
			if !neededBy(name, remove, needs) {
				continue
			}
			delete(remove, name)
			kept = append(kept, KeptMember{FullName: name, Version: idx.have[name].Version, Reason: keptRequired})
			again = true
		}
	}
	return kept
}

// neededBy reports whether a package outside remove depends on name.
func neededBy(name string, remove map[string]bool, needs map[string]map[string]string) bool {
	for other, deps := range needs {
		if _, ok := deps[name]; ok && !remove[other] {
			return true
		}
	}
	return false
}

// plannedVersions is every package the instance would hold after the change, at its version.
func plannedVersions(
	closure modresolver.Closure, idx *Index, removals []string,
) map[string]string {
	planned := make(map[string]string, len(idx.have)+len(closure.Nodes))
	for name := range idx.have {
		planned[name] = idx.have[name].Version
	}
	for _, n := range closure.Nodes {
		planned[n.FullName] = n.Version
	}
	for _, name := range removals {
		delete(planned, name)
	}
	return planned
}

// conflictsOf checks the planned result for unmet dependencies the change causes. A modpack's own
// dependencies are its membership, which a local override departs from on purpose, so they are
// not checked.
func conflictsOf(
	closure modresolver.Closure, removals []string, packs map[string]bool, idx *Index,
) []modresolver.Conflict {
	changed := map[string]bool{}
	for _, n := range closure.Nodes {
		if !n.NoOp {
			changed[n.FullName] = true
		}
	}
	for _, name := range removals {
		changed[name] = true
	}
	conflicts := modresolver.Check(plannedVersions(closure, idx, removals), changed, idx)
	return slices.DeleteFunc(conflicts, func(c modresolver.Conflict) bool { return packs[c.FullName] })
}

// installedPacks names the installed packages filed as modpacks.
func installedPacks(idx *Index) map[string]bool {
	out := map[string]bool{}
	for name := range idx.have {
		if IsPack(idx.have[name].Package) {
			out[name] = true
		}
	}
	return out
}

// PackMember is an installed package's place in an installed modpack.
type PackMember struct {
	Pack    string
	Version string
}

// PackMembership maps each installed package an installed modpack names to that pack and the
// version it pins. A package two packs name is attributed to the first by full name.
func (p *Planner) PackMembership(
	ctx context.Context, rows []store.CataloguedMod,
) (map[string]PackMember, error) {
	out := map[string]PackMember{}
	for i := range rows {
		if !IsPack(rows[i].Package) {
			continue
		}
		deps, _, ok, err := p.DB.ModVersionDependencies(ctx, rows[i].FullName, rows[i].Version, rows[i].Source)
		if err != nil {
			return nil, fmt.Errorf("read the members of %s: %w", rows[i].FullName, err)
		}
		if !ok {
			continue
		}
		for name, pin := range pins(deps) {
			if _, taken := out[name]; !taken {
				out[name] = PackMember{Pack: rows[i].FullName, Version: pin}
			}
		}
	}
	return out, nil
}

// describeConflicts is a job's refusal, one clause per unmet dependency.
func describeConflicts(conflicts []modresolver.Conflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		have := "removes it"
		if c.Have != "" {
			have = "leaves " + c.Have
		}
		parts = append(parts, fmt.Sprintf("%s %s needs %s %s or newer, and the change %s",
			c.FullName, c.Version, c.Dependency, c.Requires, have))
	}
	return strings.Join(parts, "; ")
}

func newer(candidate, installed string) bool {
	c, cOK := semver.ParseVersion(candidate)
	i, iOK := semver.ParseVersion(installed)
	return cOK && iOK && semver.Compare(c, i) > 0
}

// Newer reports whether candidate is a parseable version above installed.
func Newer(candidate, installed string) bool { return newer(candidate, installed) }

// markTransitive distinguishes the package named by the request from its closure.
func markTransitive(closure modresolver.Closure, requested string) modresolver.Closure {
	for i := range closure.Nodes {
		closure.Nodes[i].Transitive = closure.Nodes[i].FullName != requested
	}
	return closure
}

func hasNode(closure modresolver.Closure, fullName string) bool {
	for _, n := range closure.Nodes {
		if n.FullName == fullName {
			return true
		}
	}
	return false
}

// ClosureNames includes no-op dependencies because disabled installed mods cannot load.
func ClosureNames(closure modresolver.Closure) []string {
	out := make([]string, 0, len(closure.Nodes))
	for _, n := range closure.Nodes {
		out = append(out, n.FullName)
	}
	return out
}

// DisabledInClosure lists disabled installed packages reached by a planned change.
func DisabledInClosure(names []string, installed []store.InstanceMod) []string {
	off := map[string]bool{}
	for i := range installed {
		if !installed[i].Enabled {
			off[installed[i].FullName] = true
		}
	}
	var out []string
	for _, name := range names {
		if off[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
