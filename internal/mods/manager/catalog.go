package manager

import (
	"context"
	"fmt"
	"slices"

	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// IndexedPackage selects a catalogue row, preferring the requested registry.
// A nil allowed slice includes disabled registries for read-only installed views.
func IndexedPackage(
	ctx context.Context, db *store.DB, fullName string, prefer source.Source, allowed []source.Source,
) (*store.ModPackage, error) {
	rows, err := db.ModPackagesByFullName(ctx, fullName)
	if err != nil {
		return nil, fmt.Errorf("read the index row for %s: %w", fullName, err)
	}
	if allowed != nil {
		rows = slices.DeleteFunc(rows, func(row store.ModPackage) bool {
			return !slices.Contains(allowed, row.Source)
		})
	}
	if len(rows) == 0 {
		return nil, nil
	}
	for _, want := range append([]source.Source{prefer}, source.All()...) {
		for i := range rows {
			if rows[i].Source == want {
				return &rows[i], nil
			}
		}
	}
	return &rows[0], nil
}

// LatestBepInEx chooses the highest framework version from enabled registries.
func LatestBepInEx(
	ctx context.Context, db *store.DB, enabled []source.Source, prefer source.Source,
) (version string, ok bool, err error) {
	rows, err := db.ModPackagesByFullName(ctx, BepInExPack)
	if err != nil {
		return "", false, fmt.Errorf("look up %s: %w", BepInExPack, err)
	}
	var best semver.Version
	for i := range rows {
		if !slices.Contains(enabled, rows[i].Source) {
			continue
		}
		candidate := rows[i].LatestVersion
		if candidate == "" {
			continue
		}
		parsed, parsedOK := semver.ParseVersion(candidate)
		switch {
		case !ok:
		case !parsedOK:
			continue
		case semver.Compare(parsed, best) > 0:
		case parsed == best && rows[i].Source == prefer:
		default:
			continue
		}
		version, best, ok = candidate, parsed, true
	}
	return version, ok, nil
}
