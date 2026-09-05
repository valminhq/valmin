package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ModPackage is one row of mod_packages, the Thunderstore index cache (03 §6.1). Fields are
// named for what the column means rather than what the API called it: Namespace is the API's
// "owner", and Description, LatestVersion, Downloads and IconURL are derived from the package's
// version list, which is the only place the v1 listing carries them. CategoriesJSON is the
// caller's already-encoded array, so this package need not know the API type's shape.
type ModPackage struct {
	FullName       string
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
}

// ModVersion is one row of mod_versions. DependenciesJSON is the caller's already-encoded
// array, for the same reason ModPackage.CategoriesJSON is.
type ModVersion struct {
	FullName         string
	Version          string
	DependenciesJSON string
	DownloadURL      string
	FileSize         int64
}

// UpsertModPackages writes one batch of packages and their versions in a single transaction.
// The fetch and decode that produced the rows have already happened, so nothing here touches the
// network (12 §6). synced_at is stamped once through the one formatter every TIMESTAMP column
// requires (ADR-052).
func (db *DB) UpsertModPackages(ctx context.Context, packages []ModPackage, versions []ModVersion) error {
	if len(packages) == 0 && len(versions) == 0 {
		return nil
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
				full_name, namespace, name, description, latest_version,
				downloads, rating, is_deprecated, categories, icon_url, synced_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (full_name) DO UPDATE SET
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
			p.FullName, p.Namespace, p.Name, p.Description, p.LatestVersion,
			p.Downloads, p.Rating, p.IsDeprecated, p.CategoriesJSON, p.IconURL, syncedAt,
		); err != nil {
			return fmt.Errorf("upsert mod_packages %s: %w", p.FullName, err)
		}
	}

	for _, v := range versions {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mod_versions (full_name, version, dependencies, download_url, file_size)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (full_name, version) DO UPDATE SET
				dependencies = excluded.dependencies,
				download_url = excluded.download_url,
				file_size = excluded.file_size`,
			v.FullName, v.Version, v.DependenciesJSON, v.DownloadURL, v.FileSize,
		); err != nil {
			return fmt.Errorf("upsert mod_versions %s-%s: %w", v.FullName, v.Version, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mod sync batch: %w", err)
	}
	return nil
}

// ModPackageByFullName reads one mod_packages row. A missing package is (nil, nil) — the
// same not-found convention JobByID uses — so a caller can tell "does not exist" from a
// genuine read failure without inspecting the error's type.
func (db *DB) ModPackageByFullName(ctx context.Context, fullName string) (*ModPackage, error) {
	var p ModPackage
	err := db.Reader.QueryRowContext(ctx, `
		SELECT full_name, namespace, name, description, latest_version,
			downloads, rating, is_deprecated, categories, icon_url
		FROM mod_packages WHERE full_name = ?`, fullName,
	).Scan(
		&p.FullName, &p.Namespace, &p.Name, &p.Description, &p.LatestVersion,
		&p.Downloads, &p.Rating, &p.IsDeprecated, &p.CategoriesJSON, &p.IconURL,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read mod_packages %s: %w", fullName, err)
	}
	return &p, nil
}

// ModVersionsByFullName reads every mod_versions row for one package, for tests and for
// the package detail endpoint.
func (db *DB) ModVersionsByFullName(ctx context.Context, fullName string) ([]ModVersion, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT full_name, version, dependencies, download_url, file_size
		FROM mod_versions WHERE full_name = ?`, fullName)
	if err != nil {
		return nil, fmt.Errorf("read mod_versions %s: %w", fullName, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ModVersion
	for rows.Next() {
		var v ModVersion
		if err := rows.Scan(&v.FullName, &v.Version, &v.DependenciesJSON, &v.DownloadURL, &v.FileSize); err != nil {
			return nil, fmt.Errorf("scan mod_versions %s: %w", fullName, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read mod_versions %s: %w", fullName, err)
	}
	return out, nil
}

// ModVersionDependencies reads one mod_versions row's already-decoded dependency idents. ok is
// false only when that exact (full_name, version) pair is absent from the index, which is an
// unresolvable dependency rather than a package with none.
func (db *DB) ModVersionDependencies(
	ctx context.Context,
	fullName, version string,
) (deps []string, ok bool, err error) {
	var raw string
	err = db.Reader.QueryRowContext(ctx,
		`SELECT dependencies FROM mod_versions WHERE full_name = ? AND version = ?`, fullName, version,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read mod_versions dependencies %s-%s: %w", fullName, version, err)
	}
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil, false, fmt.Errorf("decode mod_versions dependencies %s-%s: %w", fullName, version, err)
	}
	return deps, true, nil
}

// SearchModPackages implements `GET /mods/search`: `LIKE` over name and description, optionally
// narrowed by category, ordered by relevance then popularity (ADR-114).
//
// Relevance is one SQL expression: a name match beats a description-only match, exact beats
// prefix beats substring among name matches, a deprecated package sorts below a live one at the
// same tier, and within a tier most-downloaded comes first. With no q that last rule is the
// whole ordering.
//
// afterSortKey and afterFullName are the previous page's last row, or "" for the first page
// (ADR-035).
func (db *DB) SearchModPackages(
	ctx context.Context,
	q, category, afterSortKey, afterFullName string,
	limit int,
) ([]ModPackage, error) {
	var where []string
	var args []any

	// The relevance tier and the ranking args that feed it. Even tiers are live packages and odd
	// ones deprecated: a boolean column is 0 or 1 in SQLite, so "+ is_deprecated" demotes within a
	// tier without needing one of its own, with or without a query.
	rank := "is_deprecated"
	if q != "" {
		esc := escapeLike(q)
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
	if category != "" {
		where = append(where, `categories LIKE ? ESCAPE '\'`)
		args = append(args, `%"`+escapeLike(category)+`"%`)
	}

	// One lexicographically-ordered string rather than three ORDER BY columns, since the keyset
	// cursor carries one sort key plus an id (ADR-035). Downloads are subtracted from the int64
	// ceiling so more of them sorts earlier under the cursor's ascending comparison, and padded to
	// 19 digits so that comparison is numeric in effect.
	sortKey := `printf('%d:%019d', ` + rank +
		`, 9223372036854775807 - COALESCE(downloads, 0))`

	query := `SELECT full_name, namespace, name, description, latest_version,
		downloads, rating, is_deprecated, categories, icon_url, ` + sortKey + ` AS sort_key
		FROM mod_packages`
	if len(where) > 0 {
		// where holds only the fixed clause literals above — never q, category or the
		// cursor, all of which travel exclusively through args as bound parameters.
		query += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: no request value reaches this string
	}

	// The cursor filters the ranked set from outside, because SQLite will not resolve a
	// SELECT alias in the WHERE that produced it, and repeating the expression would be the
	// second copy this design exists to avoid.
	query = "SELECT * FROM (" + query + ")"
	if afterSortKey != "" {
		query += " WHERE sort_key > ? OR (sort_key = ? AND full_name > ?)"
		args = append(args, afterSortKey, afterSortKey, afterFullName)
	}
	query += " ORDER BY sort_key ASC, full_name ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search mod_packages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ModPackage{}
	for rows.Next() {
		var p ModPackage
		if err := rows.Scan(
			&p.FullName, &p.Namespace, &p.Name, &p.Description, &p.LatestVersion,
			&p.Downloads, &p.Rating, &p.IsDeprecated, &p.CategoriesJSON, &p.IconURL,
			&p.SearchSortKey,
		); err != nil {
			return nil, fmt.Errorf("scan mod_packages: %w", err)
		}
		out = append(out, p)
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
// it is. ok is false when the cached index has no such version — the resolver has already
// established it does, so a false here means a sync dropped it in between.
func (db *DB) ModVersionDownload(
	ctx context.Context, fullName, version string,
) (downloadURL string, fileSize int64, ok bool, err error) {
	err = db.Reader.QueryRowContext(ctx,
		`SELECT download_url, file_size FROM mod_versions WHERE full_name = ? AND version = ?`,
		fullName, version).Scan(&downloadURL, &fileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("read mod_versions %s-%s: %w", fullName, version, err)
	}
	return downloadURL, fileSize, true, nil
}
