package manager

import (
	"context"
	"fmt"
	"slices"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// CodeMod is one package version a profile code asks for. Source is empty when the code names no
// registry.
type CodeMod struct {
	FullName string
	Version  string
	Source   string
}

// How applying a code changes a package.
const (
	ChangeInstall   = "install"
	ChangeUpgrade   = "upgrade"
	ChangeDowngrade = "downgrade"
)

// Why a package in a code is left alone.
const (
	SkipLocked           = "locked"
	SkipDisabled         = "disabled"
	SkipModpack          = "modpack"
	SkipFramework        = "framework"
	SkipOtherRegistry    = "other_registry"
	SkipNotInRegistry    = "not_in_registry"
	SkipRegistryDisabled = "registry_disabled"
	SkipUnknownRegistry  = "unknown_registry"
	SkipUnavailable      = "unavailable"
)

// CodeRow is what applying a code would do to one package. Source is the registry the package
// comes from: the installed one when it is installed. Change is empty for a package that is left
// alone, and Skip says why.
type CodeRow struct {
	FullName  string `json:"full_name"`
	Installed string `json:"installed_version"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	Change    string `json:"change,omitempty"`
	Skip      string `json:"skip,omitempty"`
}

// Actionable reports whether the row changes the server when applied.
func (r *CodeRow) Actionable() bool { return r.Change != "" && r.Skip == "" }

// DefaultCodeSelection is the rows applied unless the operator chooses otherwise: installs and
// raised versions, never a lowered one.
func DefaultCodeSelection(r *CodeRow) bool { return r.Actionable() && r.Change != ChangeDowngrade }

// CodeTargets is the update targets of the rows chosen by selected.
func CodeTargets(rows []CodeRow, selected func(*CodeRow) bool) []UpdateTarget {
	out := []UpdateTarget{}
	for i := range rows {
		r := &rows[i]
		if r.Actionable() && selected(r) {
			out = append(out, UpdateTarget{
				FullName: r.FullName, Source: r.Source, FromVersion: r.Installed, Version: r.Version,
			})
		}
	}
	return out
}

// ClassifyCode decides what applying a profile code would do to each package it lists. An
// installed package keeps its registry: a code that names another, or whose version its registry
// lacks, leaves it alone rather than reaching for the other registry. A package that is not
// installed comes from the registry the code names, else the registry that served the code,
// else any enabled one that carries the version. matching counts the packages already at the
// code's version.
func (p *Planner) ClassifyCode(
	ctx context.Context, instanceID string, mods []CodeMod, origin source.Source,
) (rows []CodeRow, matching int, err error) {
	installed, err := p.DB.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		return nil, 0, fmt.Errorf("read installed mods: %w", err)
	}
	members, err := p.PackMembership(ctx, installed)
	if err != nil {
		return nil, 0, err
	}
	have := make(map[string]*store.CataloguedMod, len(installed))
	for i := range installed {
		have[installed[i].FullName] = &installed[i]
	}
	seen := map[string]bool{}
	for _, mod := range mods {
		if seen[mod.FullName] {
			continue
		}
		seen[mod.FullName] = true
		row, matches, err := p.classifyMod(ctx, mod, have[mod.FullName], members, origin)
		if err != nil {
			return nil, 0, err
		}
		if matches {
			matching++
			continue
		}
		rows = append(rows, row)
	}
	return rows, matching, nil
}

// classifyMod is ClassifyCode for one package. matches is true for an installed package already
// at the code's version from the registry the code names.
func (p *Planner) classifyMod(
	ctx context.Context, mod CodeMod, have *store.CataloguedMod, members map[string]PackMember,
	origin source.Source,
) (row CodeRow, matches bool, err error) {
	row = CodeRow{FullName: mod.FullName, Version: mod.Version, Source: mod.Source}
	named, hasName := source.ByName(mod.Source)
	switch {
	case mod.Source != "" && !hasName:
		row.Skip = SkipUnknownRegistry
	case hasName && !slices.Contains(p.Enabled, named):
		row.Skip = SkipRegistryDisabled
	case have == nil:
		err = p.classifyNew(ctx, &row, named, hasName, origin)
	default:
		matches, err = p.classifyInstalled(ctx, &row, have, members, named, hasName)
	}
	return row, matches, err
}

// classifyInstalled decides a package that is installed. It reports true when the package is
// already at the code's version.
func (p *Planner) classifyInstalled(
	ctx context.Context, row *CodeRow, have *store.CataloguedMod, members map[string]PackMember,
	named source.Source, hasName bool,
) (matches bool, err error) {
	row.Installed, row.Source = have.Version, have.Source.String()
	member, inPack := members[row.FullName]
	switch {
	case hasName && named != have.Source:
		row.Skip = SkipOtherRegistry
	case have.Version == row.Version:
		return true, nil
	case row.FullName == BepInExPack:
		row.Skip = SkipFramework
	case !have.Enabled:
		row.Skip = SkipDisabled
	case have.Locked:
		row.Skip = SkipLocked
	case IsPack(have.Package) || inPack && FollowsPack(&have.InstanceMod, member.Version):
		row.Skip = SkipModpack
	case !slices.Contains(p.Enabled, have.Source):
		row.Skip = SkipRegistryDisabled
	default:
		return false, p.classifyChange(ctx, row, have.Version, have.Source)
	}
	return false, nil
}

// classifyChange marks an installed package as raised or lowered to the code's version, or
// leaves it alone when its registry does not carry that version.
func (p *Planner) classifyChange(ctx context.Context, row *CodeRow, installed string, src source.Source) error {
	carried, err := p.carries(ctx, row.FullName, row.Version, src)
	switch {
	case err != nil:
		return err
	case !carried:
		row.Skip = SkipNotInRegistry
	case newer(installed, row.Version):
		row.Change = ChangeDowngrade
	default:
		row.Change = ChangeUpgrade
	}
	return nil
}

// classifyNew picks the registry for a package that is not installed. The mod framework is never
// installed from a code: the panel installs the newest one with the first mod.
func (p *Planner) classifyNew(
	ctx context.Context, row *CodeRow, named source.Source, hasName bool, origin source.Source,
) error {
	if row.FullName == BepInExPack {
		row.Skip = SkipFramework
		return nil
	}
	candidates := []source.Source{origin}
	for _, src := range source.All() {
		if src != origin {
			candidates = append(candidates, src)
		}
	}
	if hasName {
		candidates = []source.Source{named}
	}
	for _, src := range candidates {
		if !slices.Contains(p.Enabled, src) {
			continue
		}
		carried, err := p.carries(ctx, row.FullName, row.Version, src)
		if err != nil {
			return err
		}
		if carried {
			row.Source, row.Change = src.String(), ChangeInstall
			return nil
		}
	}
	row.Skip = SkipUnavailable
	return nil
}

// carries reports whether a registry has the exact version of a package.
func (p *Planner) carries(ctx context.Context, fullName, version string, src source.Source) (bool, error) {
	_, _, ok, err := p.DB.ModVersionDependenciesFrom(
		ctx, fullName, version, source.Source{}, []source.Source{src})
	if err != nil {
		return false, fmt.Errorf("look up %s %s in %s: %w", fullName, version, src, err)
	}
	return ok, nil
}

// MaxCodeTargets is the most packages one code applies at once.
const MaxCodeTargets = 1000

// CheckCodeTargets validates the confirmed targets of a code against the installed rows. A target
// may install a package or move it either way, but never to another registry than the installed
// one, and never touches a locked, disabled or modpack-managed package or the framework.
func (p *Planner) CheckCodeTargets(
	ctx context.Context, instanceID string, targets []UpdateTarget,
) ([]UpdateTarget, []updateIssue, error) {
	if len(targets) == 0 {
		return nil, []updateIssue{{Field: "targets", Message: "Name at least one mod to change."}}, nil
	}
	if len(targets) > MaxCodeTargets {
		return nil, []updateIssue{{Field: "targets", Message: "Too many mods to change at once."}}, nil
	}
	installed, err := p.DB.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read installed mods: %w", err)
	}
	members, err := p.PackMembership(ctx, installed)
	if err != nil {
		return nil, nil, err
	}
	have := make(map[string]*store.CataloguedMod, len(installed))
	for i := range installed {
		have[installed[i].FullName] = &installed[i]
	}
	seen := map[string]bool{}
	out := make([]UpdateTarget, 0, len(targets))
	var issues []updateIssue
	for i, target := range targets {
		field := fmt.Sprintf("targets[%d]", i)
		message := codeTargetRefusal(target, have[target.FullName], members, p.Enabled, seen[target.FullName])
		if message != "" {
			issues = append(issues, updateIssue{Field: field, Message: message})
		} else {
			from := ""
			if row := have[target.FullName]; row != nil {
				from = row.Version
			}
			out = append(out, UpdateTarget{
				FullName: target.FullName, Source: target.Source, FromVersion: from, Version: target.Version,
			})
		}
		seen[target.FullName] = true
	}
	return out, issues, nil
}

// codeTargetRefusal is why a target cannot be applied, or "" when it can.
func codeTargetRefusal(
	target UpdateTarget, row *store.CataloguedMod, members map[string]PackMember,
	enabled []source.Source, duplicate bool,
) string {
	src, known := source.ByName(target.Source)
	switch {
	case duplicate:
		return target.FullName + " is named twice."
	case !known:
		return target.FullName + " names no mod registry this panel knows."
	case !slices.Contains(enabled, src):
		return src.String() + " is not enabled on this panel."
	case target.FullName == BepInExPack:
		return "The mod framework is managed by the panel."
	case row == nil:
		return ""
	case target.Source != row.Source.String():
		return target.FullName + " was installed from " + row.Source.String() + " and changes there only."
	case target.Version == row.Version:
		return target.FullName + " is already at " + row.Version + "."
	case !row.Enabled:
		return target.FullName + " is disabled. Enable it before changing it."
	case row.Locked:
		return target.FullName + " is locked. Unlock it before changing it."
	case IsPack(row.Package):
		return target.FullName + " is a modpack. Install its new version instead."
	}
	if member, ok := members[target.FullName]; ok && FollowsPack(&row.InstanceMod, member.Version) {
		return target.FullName + " follows its modpack."
	}
	return ""
}
