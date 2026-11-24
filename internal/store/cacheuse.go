package store

import (
	"context"
	"fmt"
)

// ModArchive names one registry package version, the key of a cached zip.
type ModArchive struct {
	Source   string
	FullName string
	Version  string
}

// ArchiveUse is who still names a package version: the servers that have it installed and the
// number of saved setups that hold it.
type ArchiveUse struct {
	InstanceIDs []string
	Setups      int
}

// ModArchiveUse maps every package version a server has installed or a saved setup holds to
// its users. A version absent from the map is named by nothing.
func (db *DB) ModArchiveUse(ctx context.Context) (map[ModArchive]*ArchiveUse, error) {
	use := map[ModArchive]*ArchiveUse{}
	if err := db.installedArchives(ctx, use); err != nil {
		return nil, err
	}
	if err := db.setupArchives(ctx, use); err != nil {
		return nil, err
	}
	return use, nil
}

// installedArchives adds each server's installed package versions to use.
func (db *DB) installedArchives(ctx context.Context, use map[ModArchive]*ArchiveUse) error {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT source, full_name, version, instance_id FROM instance_mods ORDER BY instance_id`)
	if err != nil {
		return fmt.Errorf("list installed mod archives: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a ModArchive
		var instanceID string
		if err := rows.Scan(&a.Source, &a.FullName, &a.Version, &instanceID); err != nil {
			return fmt.Errorf("scan installed mod archive: %w", err)
		}
		u := archiveUse(use, a)
		u.InstanceIDs = append(u.InstanceIDs, instanceID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list installed mod archives: %w", err)
	}
	return nil
}

// setupArchives adds the package versions held by saved setup snapshots to use. A snapshot mod
// without a source predates registries other than Thunderstore.
func (db *DB) setupArchives(ctx context.Context, use map[ModArchive]*ArchiveUse) error {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(json_extract(m.value, '$.source'), ''), 'thunderstore'),
		       json_extract(m.value, '$.full_name'), json_extract(m.value, '$.version'),
		       COUNT(DISTINCT s.id)
		FROM saved_setups s, json_each(s.snapshot_json, '$.mods') m
		GROUP BY 1, 2, 3`)
	if err != nil {
		return fmt.Errorf("list saved setup mod archives: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a ModArchive
		var setups int
		if err := rows.Scan(&a.Source, &a.FullName, &a.Version, &setups); err != nil {
			return fmt.Errorf("scan saved setup mod archive: %w", err)
		}
		archiveUse(use, a).Setups = setups
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list saved setup mod archives: %w", err)
	}
	return nil
}

// archiveUse returns a's entry in use, adding an empty one first.
func archiveUse(use map[ModArchive]*ArchiveUse, a ModArchive) *ArchiveUse {
	if use[a] == nil {
		use[a] = &ArchiveUse{}
	}
	return use[a]
}

// SetupArtifactUse maps every package and config digest a saved setup holds to the number of
// setups holding it.
func (db *DB) SetupArtifactUse(ctx context.Context) (map[string]int, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT sha256, COUNT(DISTINCT setup_id) FROM (
			SELECT sha256, setup_id FROM saved_setup_artifacts
			UNION ALL SELECT sha256, setup_id FROM saved_setup_configs
		) GROUP BY sha256`)
	if err != nil {
		return nil, fmt.Errorf("count setup artifact use: %w", err)
	}
	defer func() { _ = rows.Close() }()
	use := map[string]int{}
	for rows.Next() {
		var digest string
		var setups int
		if err := rows.Scan(&digest, &setups); err != nil {
			return nil, fmt.Errorf("scan setup artifact use: %w", err)
		}
		use[digest] = setups
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count setup artifact use: %w", err)
	}
	return use, nil
}
