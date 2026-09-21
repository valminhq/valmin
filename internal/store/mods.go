package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/mods/source"
)

// ModPackage is one row of mod_packages, the registry index cache (03 §6.1). Fields are
// named for what the column means rather than what the API called it: Namespace is the API's
// "owner", and Description, LatestVersion, Downloads and IconURL are derived from the package's
// version list, which is the only place the v1 listing carries them. CategoriesJSON is the
// caller's already-encoded array, so this package need not know the API type's shape.
//
// Source is half the identity: the same full_name exists on more than one registry, each with
// its own versions and bytes (B14).
type ModPackage struct {
	FullName       string
	Source         source.Source
	Namespace      string
	Name           string
	Description    string
	LatestVersion  string
	Downloads      int64
	Rating         int
	IsDeprecated   bool
	CategoriesJSON string
	IconURL        string

	// SearchSortKey is the opaque keyset cursor for the row's position in a search result, set
	// only by SearchModPackages. Computed in SQL so one expression defines both the ordering and
	// the cursor and they cannot drift apart. Callers echo it back and never parse it.
	SearchSortKey string

	// SearchRowKey is the cursor's tiebreaker within one sort key, set only by
	// SearchModPackages. full_name alone no longer identifies a row, so the key pairs it with
	// the registry; without it a page boundary could repeat or skip a row wherever two
	// registries carry one package.
	SearchRowKey string
}

// ModVersion is one row of mod_versions. DependenciesJSON is the caller's already-encoded
// array, for the same reason ModPackage.CategoriesJSON is.
type ModVersion struct {
	FullName         string
	Source           source.Source
	Version          string
	DependenciesJSON string
	DownloadURL      string
	FileSize         int64
}

// UpsertModPackages writes one batch of packages and their versions in a single transaction.
// The fetch and decode that produced the rows have already happened, so nothing here touches the
// network (12 §6). synced_at is stamped once through the one formatter every TIMESTAMP column
// requires (ADR-052).
//
// Every row must name its registry. An unspecified source is rejected rather than written,
// because a row that claims no registry is one no sync will ever refresh and no install can
// resolve bytes for (B14).
func (db *DB) UpsertModPackages(ctx context.Context, packages []ModPackage, versions []ModVersion) error {
	if len(packages) == 0 && len(versions) == 0 {
		return nil
	}
	for i := range packages {
		if packages[i].Source == (source.Source{}) {
			return fmt.Errorf("upsert mod_packages %s: no registry named", packages[i].FullName)
		}
	}
	for i := range versions {
		if versions[i].Source == (source.Source{}) {
			return fmt.Errorf("upsert mod_versions %s-%s: no registry named",
				versions[i].FullName, versions[i].Version)
		}
	}
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mod sync batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	syncedAt := Now()
	for i := range packages {
		p := &packages[i]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mod_packages (
				full_name, source, namespace, name, description, latest_version,
				downloads, rating, is_deprecated, categories, icon_url, synced_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (full_name, source) DO UPDATE SET
				namespace = excluded.namespace,
				name = excluded.name,
				description = excluded.description,
				latest_version = excluded.latest_version,
				downloads = excluded.downloads,
				rating = excluded.rating,
				is_deprecated = excluded.is_deprecated,
				categories = excluded.categories,
				icon_url = excluded.icon_url,
				synced_at = excluded.synced_at`,
			p.FullName, p.Source.String(), p.Namespace, p.Name, p.Description, p.LatestVersion,
			p.Downloads, p.Rating, p.IsDeprecated, p.CategoriesJSON, p.IconURL, syncedAt,
		); err != nil {
			return fmt.Errorf("upsert mod_packages %s: %w", p.FullName, err)
		}
	}

	for _, v := range versions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mod_versions (full_name, source, version, dependencies, download_url, file_size)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (full_name, source, version) DO UPDATE SET
				dependencies = excluded.dependencies,
				download_url = excluded.download_url,
				file_size = excluded.file_size`,
			v.FullName, v.Source.String(), v.Version, v.DependenciesJSON, v.DownloadURL, v.FileSize,
		); err != nil {
			return fmt.Errorf("upsert mod_versions %s-%s: %w", v.FullName, v.Version, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mod sync batch: %w", err)
	}
	return nil
}

// scanSource resolves a persisted registry name back to its constant. A name no build
// recognises is a named error rather than a silently zero value, since the column feeds the
// download URL, the cache root and the install's recorded origin (B14).
func scanSource(table, name string) (source.Source, error) {
	s, ok := source.ByName(name)
	if !ok {
		return source.Source{}, fmt.Errorf("%s: unknown registry %q", table, name)
	}
	return s, nil
}

// ModPackagesByFullName reads one package's row from every registry that carries it, ordered
// by registry. An empty slice is a package no registry has, which callers report as not found;
// more than one row is a package both carry, and the caller chooses between them.
func (db *DB) ModPackagesByFullName(ctx context.Context, fullName string) ([]ModPackage, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT full_name, source, namespace, name, description, latest_version,
			downloads, rating, is_deprecated, categories, icon_url
		FROM mod_packages WHERE full_name = ? ORDER BY source`, fullName)
	if err != nil {
		return nil, fmt.Errorf("read mod_packages %s: %w", fullName, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ModPackage
	for rows.Next() {
		var (
			p   ModPackage
			src string
		)
		if err := rows.Scan(
			&p.FullName, &src, &p.Namespace, &p.Name, &p.Description, &p.LatestVersion,
			&p.Downloads, &p.Rating, &p.IsDeprecated, &p.CategoriesJSON, &p.IconURL,
		); err != nil {
			return nil, fmt.Errorf("scan mod_packages %s: %w", fullName, err)
		}
		if p.Source, err = scanSource("mod_packages", src); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mod_packages %s: %w", fullName, err)
	}
	return out, nil
}

// ModVersionsByFullName reads every mod_versions row for one package, for tests and for
// the package detail endpoint.
func (db *DB) ModVersionsByFullName(ctx context.Context, fullName string) ([]ModVersion, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT full_name, source, version, dependencies, download_url, file_size
		FROM mod_versions WHERE full_name = ? ORDER BY source, version`, fullName)
	if err != nil {
		return nil, fmt.Errorf("read mod_versions %s: %w", fullName, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ModVersion
	for rows.Next() {
		var (
			v   ModVersion
			src string
		)
		if err := rows.Scan(
			&v.FullName, &src, &v.Version, &v.DependenciesJSON, &v.DownloadURL, &v.FileSize,
		); err != nil {
			return nil, fmt.Errorf("scan mod_versions %s: %w", fullName, err)
		}
		if v.Source, err = scanSource("mod_versions", src); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mod_versions %s: %w", fullName, err)
	}
	return out, nil
}

// ModVersionDependencies reads one version's already-decoded dependency idents, and reports
// which registry answered. A dependency ident names no registry (03 §6.2), so prefer decides
// between two registries carrying the version and the other one still answers when the
// preferred does not — a closure spanning both registries must resolve, not fail.
//
// ok is false only when no registry has that exact (full_name, version) pair, which is an
// unresolvable dependency rather than a package with none.
func (db *DB) ModVersionDependencies(
	ctx context.Context,
	fullName, version string,
	prefer source.Source,
) (deps []string, foundIn source.Source, ok bool, err error) {
	return db.ModVersionDependenciesFrom(ctx, fullName, version, prefer, source.All())
}

// ModVersionDependenciesFrom selects a version only from allowed registries, preferring
// prefer when it is allowed. An empty allowed list cannot resolve a version.
func (db *DB) ModVersionDependenciesFrom(
	ctx context.Context, fullName, version string, prefer source.Source, allowed []source.Source,
) (deps []string, foundIn source.Source, ok bool, err error) {
	if len(allowed) == 0 {
		return nil, source.Source{}, false, nil
	}
	marks := make([]string, 0, len(allowed))
	args := []any{fullName, version}
	for _, src := range allowed {
		marks = append(marks, "?")
		args = append(args, src.String())
	}
	args = append(args, prefer.String())
	var raw, src string
	err = db.Reader.QueryRowContext(ctx, `
		SELECT dependencies, source FROM mod_versions
		WHERE full_name = ? AND version = ? AND source IN (`+strings.Join(marks, ",")+`)
		ORDER BY source <> ?, source
		LIMIT 1`, args...,
	).Scan(&raw, &src)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, source.Source{}, false, nil
	}
	if err != nil {
		return nil, source.Source{}, false,
			fmt.Errorf("read mod_versions dependencies %s-%s: %w", fullName, version, err)
	}
	if foundIn, err = scanSource("mod_versions", src); err != nil {
		return nil, source.Source{}, false, err
	}
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil, source.Source{}, false,
			fmt.Errorf("decode mod_versions dependencies %s-%s: %w", fullName, version, err)
	}
	return deps, foundIn, true, nil
}

// ModSearch is one search request. It is a struct rather than a parameter list because the
// two cursor halves are adjacent strings, and an accidental swap between them is a silent
// pagination fault rather than a compile error.
type ModSearch struct {
	Query    string
	Category string
	// Source narrows the result to one registry. The zero value searches every registry, which
	// is what the UI's "all" offers: an explicit "all" would be a third name for the absence
	// of a filter.
	Source source.Source
	// Sources limits browsing to enabled registries; nil leaves the query unrestricted.
	Sources []source.Source
	// AfterSortKey and AfterRowKey are the previous page's last row, both empty on the first
	// page (ADR-035).
	AfterSortKey string
	AfterRowKey  string
	Limit        int
}

// SearchModPackages implements `GET /mods/search`: `LIKE` over name and description, optionally
// narrowed by category and by registry, ordered by relevance then popularity (ADR-114).
//
// Relevance is one SQL expression: a name match beats a description-only match, exact beats
// prefix beats substring among name matches, a deprecated package sorts below a live one at the
// same tier, and within a tier most-downloaded comes first. With no q that last rule is the
// whole ordering.
func (db *DB) SearchModPackages(ctx context.Context, p *ModSearch) ([]ModPackage, error) {
	var where []string
	var args []any

	// The relevance tier and the ranking args that feed it. Even tiers are live packages and odd
	// ones deprecated: a boolean column is 0 or 1 in SQLite, so "+ is_deprecated" demotes within a
	// tier without needing one of its own, with or without a query.
	rank := "is_deprecated"
	if p.Query != "" {
		esc := escapeLike(p.Query)
		rank = `(CASE
			WHEN name LIKE ? ESCAPE '\' THEN 0
			WHEN name LIKE ? ESCAPE '\' THEN 2
			WHEN name LIKE ? ESCAPE '\' THEN 4
			ELSE 6
		END + is_deprecated)`
		args = append(args, esc, esc+`%`, `%`+esc+`%`)

		pattern := `%` + esc + `%`
		where = append(where, `(name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	if p.Category != "" {
		where = append(where, `categories LIKE ? ESCAPE '\'`)
		args = append(args, `%"`+escapeLike(p.Category)+`"%`)
	}
	if p.Sources != nil {
		// SQLite accepts IN () and matches no rows when every registry is disabled.
		marks := make([]string, 0, len(p.Sources))
		for _, src := range p.Sources {
			marks = append(marks, "?")
			args = append(args, src.String())
		}
		where = append(where, "source IN ("+strings.Join(marks, ",")+")")
	}
	if p.Source != (source.Source{}) {
		where = append(where, `source = ?`)
		args = append(args, p.Source.String())
	}

	// One lexicographically-ordered string rather than three ORDER BY columns, since the keyset
	// cursor carries one sort key plus an id (ADR-035). Downloads are subtracted from the int64
	// ceiling so more of them sorts earlier under the cursor's ascending comparison, and padded to
	// 19 digits so that comparison is numeric in effect.
	sortKey := `printf('%d:%019d', ` + rank +
		`, 9223372036854775807 - COALESCE(downloads, 0))`

	// The cursor's tiebreaker within one sort key. full_name alone is no longer unique — two
	// registries carry one package — so the key pairs it with the source, separated by the unit
	// separator, which sorts below every character a name may contain and so cannot be forged
	// by a package called "a\x1fb".
	rowKey := `full_name || char(31) || source`

	query := `SELECT full_name, source, namespace, name, description, latest_version,
		downloads, rating, is_deprecated, categories, icon_url, ` + sortKey + ` AS sort_key, ` +
		rowKey + ` AS row_key
		FROM mod_packages`
	if len(where) > 0 {
		// where holds only the fixed clause literals above — never q, category, the source or
		// the cursor, all of which travel exclusively through args as bound parameters.
		query += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: no request value reaches this string
	}

	// The cursor filters the ranked set from outside, because SQLite will not resolve a
	// SELECT alias in the WHERE that produced it, and repeating the expression would be the
	// second copy this design exists to avoid.
	query = "SELECT * FROM (" + query + ")"
	if p.AfterSortKey != "" {
		query += " WHERE sort_key > ? OR (sort_key = ? AND row_key > ?)"
		args = append(args, p.AfterSortKey, p.AfterSortKey, p.AfterRowKey)
	}
	query += " ORDER BY sort_key ASC, row_key ASC LIMIT ?"
	args = append(args, p.Limit)

	rows, err := db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search mod_packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ModPackage{}
	for rows.Next() {
		var (
			pkg ModPackage
			src string
		)
		if err := rows.Scan(
			&pkg.FullName, &src, &pkg.Namespace, &pkg.Name, &pkg.Description, &pkg.LatestVersion,
			&pkg.Downloads, &pkg.Rating, &pkg.IsDeprecated, &pkg.CategoriesJSON, &pkg.IconURL,
			&pkg.SearchSortKey, &pkg.SearchRowKey,
		); err != nil {
			return nil, fmt.Errorf("scan mod_packages: %w", err)
		}
		if pkg.Source, err = scanSource("mod_packages", src); err != nil {
			return nil, err
		}
		out = append(out, pkg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search mod_packages: %w", err)
	}
	return out, nil
}

// escapeLike escapes a LIKE pattern's three special characters so a term containing a literal
// "%" or "_" matches literally. A literal `"` is not escaped, so a category name containing one
// would not reliably match its JSON-encoded form in the `categories` column; Thunderstore's
// category taxonomy is curated and contains none.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ModVersionDownload reads where one exact version's zip lives and how big the index says
// it is. The registry is part of the lookup, never a preference: two registries can serve
// different bytes under one full_name-version, and the caller has already chosen which one
// this install is taking (B14).
//
// ok is false when the cached index has no such version — the resolver has already
// established it does, so a false here means a sync dropped it in between.
func (db *DB) ModVersionDownload(
	ctx context.Context, fullName, version string, src source.Source,
) (downloadURL string, fileSize int64, ok bool, err error) {
	err = db.Reader.QueryRowContext(ctx, `
		SELECT download_url, file_size FROM mod_versions
		WHERE full_name = ? AND source = ? AND version = ?`,
		fullName, src.String(), version).Scan(&downloadURL, &fileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("read mod_versions %s-%s: %w", fullName, version, err)
	}
	return downloadURL, fileSize, true, nil
}
