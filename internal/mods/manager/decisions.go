package manager

import (
	"context"
	"fmt"
	"slices"
	"sort"

	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

// RequiredByError is an uninstall that another installed package depends on.
type RequiredByError struct {
	FullName string
	By       []string
}

func (e *RequiredByError) Error() string {
	return fmt.Sprintf("%s is required by %v", e.FullName, e.By)
}

// NotInstalledError is a full name that is not installed on this instance. It is a 404 and
// not a 422: from the caller's side the resource simply is not there (D2, ADR-038).
type NotInstalledError struct{ FullName string }

func (e *NotInstalledError) Error() string { return e.FullName + " is not installed" }

// RemovalSet is what an uninstall will actually remove: the named package, plus the dependencies
// nothing else needs if the request asked for them.
//
// The dependent check is a refusal rather than a cascade: removing a package another installed
// one needs would leave that one installed and unloadable.
func (p *Planner) RemovalSet(
	ctx context.Context, instanceID, fullName string, removeOrphans bool,
) ([]string, error) {
	rows, err := p.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	installed := make(map[string]*store.InstanceMod, len(rows))
	for i := range rows {
		installed[rows[i].FullName] = &rows[i]
	}
	if installed[fullName] == nil {
		return nil, &NotInstalledError{FullName: fullName}
	}

	needs, err := p.dependencyEdges(ctx, rows)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]bool, len(rows))
	for name := range installed {
		remaining[name] = true
	}
	delete(remaining, fullName)

	if by := requiredBy(fullName, remaining, needs); len(by) > 0 {
		return nil, &RequiredByError{FullName: fullName, By: by}
	}
	names := []string{fullName}
	if removeOrphans {
		names = append(names, orphansOf(installed, remaining, needs)...)
	}
	return names, nil
}

// dependencyEdges maps each installed package to the full names it depends on, read from the
// cached index at the installed version. A version the index no longer carries contributes no
// edges, which is the honest answer and keeps one stale row from blocking every uninstall.
func (p *Planner) dependencyEdges(ctx context.Context, rows []store.InstanceMod) (map[string][]string, error) {
	needs := make(map[string][]string, len(rows))
	for i := range rows {
		deps, _, ok, err := p.DB.ModVersionDependencies(
			ctx, rows[i].FullName, rows[i].Version, rows[i].Source)
		if err != nil {
			return nil, fmt.Errorf("read the dependencies of %s-%s: %w",
				rows[i].FullName, rows[i].Version, err)
		}
		if !ok {
			continue
		}
		for _, dep := range deps {
			depName, _, ok := modresolver.ParseDependency(dep)
			if !ok {
				continue
			}
			needs[rows[i].FullName] = append(needs[rows[i].FullName], depName)
		}
	}
	return needs, nil
}

// requiredBy names the packages still installed that depend on fullName.
func requiredBy(fullName string, remaining map[string]bool, needs map[string][]string) []string {
	var by []string
	for name := range remaining {
		for _, dep := range needs[name] {
			if dep == fullName {
				by = append(by, name)
				break
			}
		}
	}
	sort.Strings(by)
	return by
}

// orphansOf is every remaining `dependency` row that nothing remaining needs, to a fixed point,
// since removing one orphan can orphan the package it pulled in. It mutates remaining as it
// goes, so each pass sees the set as the decided removals would leave it.
func orphansOf(
	installed map[string]*store.InstanceMod, remaining map[string]bool, needs map[string][]string,
) []string {
	var orphans []string
	for {
		var found []string
		for name := range remaining {
			if installed[name].InstalledAs != store.InstalledDependency {
				continue
			}
			if len(requiredBy(name, remaining, needs)) == 0 {
				found = append(found, name)
			}
		}
		if len(found) == 0 {
			sort.Strings(orphans)
			return orphans
		}
		for _, name := range found {
			delete(remaining, name)
		}
		orphans = append(orphans, found...)
	}
}

// ToggleRefusal is a disable or enable the dependency graph does not allow. It is a 409 naming
// the packages in the way, like an uninstall another mod still needs.
type ToggleRefusal struct {
	Detail string
	Names  []string
	Reason string
}

func (e *ToggleRefusal) Error() string { return e.Reason }

// CheckToggle decides whether fullName may move to enable. Disabling is refused for the mod
// loader itself, and while an enabled mod depends on the package; enabling is refused while the
// package depends on a disabled one. Either would leave an enabled mod BepInEx cannot load, and
// the operator would be hunting a failure the panel made.
func (p *Planner) CheckToggle(
	ctx context.Context, rows []store.InstanceMod, fullName string, enable bool,
) error {
	if !enable && fullName == BepInExPack {
		return &ToggleRefusal{
			Detail: "reason", Reason: "BepInEx is the mod loader; disabling it disables every mod. " +
				"Disable the mods themselves instead.",
		}
	}
	needs, err := p.dependencyEdges(ctx, rows)
	if err != nil {
		return err
	}
	enabled := make(map[string]bool, len(rows))
	installed := make(map[string]bool, len(rows))
	for i := range rows {
		installed[rows[i].FullName] = true
		enabled[rows[i].FullName] = rows[i].Enabled
	}

	if !enable {
		remaining := map[string]bool{}
		for name, on := range enabled {
			if on && name != fullName {
				remaining[name] = true
			}
		}
		if by := requiredBy(fullName, remaining, needs); len(by) > 0 {
			return &ToggleRefusal{
				Detail: "required_by", Names: by,
				Reason: fmt.Sprintf("%s is needed by %v, which are enabled", fullName, by),
			}
		}
		return nil
	}

	var off []string
	for _, dep := range needs[fullName] {
		if installed[dep] && !enabled[dep] && !slices.Contains(off, dep) {
			off = append(off, dep)
		}
	}
	sort.Strings(off)
	if len(off) > 0 {
		return &ToggleRefusal{
			Detail: "disabled", Names: off,
			Reason: fmt.Sprintf("%s needs %v, which are disabled", fullName, off),
		}
	}
	return nil
}
