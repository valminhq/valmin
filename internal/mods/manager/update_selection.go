package manager

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// ListingStarts returns the last complete listing start for each registry.
func ListingStarts(ctx context.Context, db *store.DB) (map[source.Source]time.Time, error) {
	out := map[source.Source]time.Time{}
	for _, src := range source.All() {
		var stamp string
		ok, err := db.KVGet(ctx, ListingStartedKey(src), &stamp)
		if err != nil {
			return nil, fmt.Errorf("read the listing start of %s: %w", src, err)
		}
		if !ok {
			continue
		}
		started, err := store.ParseTime(stamp)
		if err != nil {
			return nil, fmt.Errorf("read the listing start of %s: %w", src, err)
		}
		out[src] = started
	}
	return out, nil
}

// Unlisted reports whether the installed registry's last complete listing omitted a package.
func Unlisted(c *store.CataloguedMod, starts map[source.Source]time.Time) bool {
	started, ok := starts[c.Source]
	if !ok {
		return false
	}
	if c.Package == nil {
		return true
	}
	listed, err := store.ParseTime(c.ListedAt)
	return err == nil && listed.Before(started)
}

// ModUpdateVersion offers only a newer version from the installed registry.
func ModUpdateVersion(mod *store.InstanceMod, pkg *store.ModPackage) string {
	if pkg == nil || pkg.Source != mod.Source {
		return ""
	}
	installed, installedOK := semver.ParseVersion(mod.Version)
	latest, latestOK := semver.ParseVersion(pkg.LatestVersion)
	if installedOK && latestOK && semver.Compare(latest, installed) > 0 {
		return pkg.LatestVersion
	}
	return ""
}

// PendingUpdates selects packages that can move to a newer indexed version.
func (p *Planner) PendingUpdates(ctx context.Context, instanceID string) ([]UpdateTarget, error) {
	rows, err := p.DB.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	starts, err := ListingStarts(ctx, p.DB)
	if err != nil {
		return nil, err
	}
	members, err := p.PackMembership(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := []UpdateTarget{}
	for i := range rows {
		member, inPack := members[rows[i].FullName]
		if !rows[i].Enabled || rows[i].Locked || IsPack(rows[i].Package) ||
			inPack && FollowsPack(&rows[i].InstanceMod, member.Version) ||
			!slices.Contains(p.Enabled, rows[i].Source) || Unlisted(&rows[i], starts) {
			continue
		}
		version := ModUpdateVersion(&rows[i].InstanceMod, rows[i].Package)
		if version == "" {
			continue
		}
		out = append(out, UpdateTarget{
			FullName: rows[i].FullName, Source: rows[i].Source.String(),
			FromVersion: rows[i].Version, Version: version,
		})
	}
	return out, nil
}

// updateIssue describes one invalid confirmed update target.
type updateIssue struct {
	Field   string
	Message string
}

// CheckUpdateTargets validates the confirmed versions against current installed rows.
func (p *Planner) CheckUpdateTargets(
	ctx context.Context, instanceID string, targets []UpdateTarget,
) ([]UpdateTarget, []updateIssue, error) {
	if len(targets) == 0 {
		return nil, []updateIssue{{Field: "targets", Message: "Name at least one mod to update."}}, nil
	}
	rows, err := p.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read installed mods: %w", err)
	}
	byName := make(map[string]*store.InstanceMod, len(rows))
	for i := range rows {
		byName[rows[i].FullName] = &rows[i]
	}
	seen := map[string]bool{}
	out := make([]UpdateTarget, 0, len(targets))
	var issues []updateIssue
	for i, target := range targets {
		field := fmt.Sprintf("targets[%d]", i)
		row := byName[target.FullName]
		message := ""
		switch {
		case seen[target.FullName]:
			message = target.FullName + " is named twice."
		case row == nil:
			message = target.FullName + " is not installed on this server."
		case !row.Enabled:
			message = target.FullName + " is disabled. Enable it before updating it."
		case row.Locked:
			message = target.FullName + " is locked. Unlock it before updating it."
		case target.Source != row.Source.String():
			message = target.FullName + " was installed from " + row.Source.String() + " and updates from there only."
		case !slices.Contains(p.Enabled, row.Source):
			message = row.Source.String() + " is not enabled on this panel."
		case !newer(target.Version, row.Version):
			message = target.Version + " is not newer than the installed " + row.Version + "."
		default:
			out = append(out, UpdateTarget{
				FullName: target.FullName, Source: target.Source,
				FromVersion: row.Version, Version: target.Version,
			})
		}
		if message != "" {
			issues = append(issues, updateIssue{Field: field, Message: message})
		}
		seen[target.FullName] = true
	}
	return out, issues, nil
}
