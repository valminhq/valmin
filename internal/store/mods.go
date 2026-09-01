package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ModPackage is one row of mod_packages — the Thunderstore index cache (03 §6.1). Fields
// are already resolved to what the column means, not what the API called it: Namespace is
// the API's "owner", and Description/LatestVersion/Downloads/IconURL are derived from the
// package's own version list, because the v1 listing carries none of them at the top
// level (found writing WP-M2-02; the panel's schema was sketched before anyone had read a
// real response). CategoriesJSON is the caller's already-encoded array, the same
// division of labour CreateInvite uses for grant_perms — this package does not need to
// know thunderstore.Package's shape to store it.
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

// UpsertModPackages writes one batch of packages and their versions in a single
// transaction (12 §6: the transaction wraps the write, and everything that produced these
// rows — the HTTP fetch, the JSON decode — has already happened by the time this is
// called, so nothing here touches the network). synced_at is stamped once, via the one
// formatter every TIMESTAMP column requires (ADR-052) — a bare time.Now().Format would
// sort wrong against every other timestamp in the database.
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
// WP-03's package detail endpoint.
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

// SearchModPackages is 04 §3's `GET /mods/search`: `LIKE` over name and description
// (`06 §1`'s "boring mechanism" decision over FTS5), optionally narrowed by category.
// Ordered alphabetically by name with full_name as the keyset tie-breaker — name alone is
// not unique across namespaces, and ties are otherwise routine (e.g. every
// freshly-synced, zero-download package would tie under a popularity order).
//
// afterName/afterFullName are the previous page's last row, or "" for the first page —
// ADR-035's keyset cursor, the same (sortKey, id) shape ListJobsForInstance uses.
func (db *DB) SearchModPackages(
	ctx context.Context,
	q, category, afterName, afterFullName string,
	limit int,
) ([]ModPackage, error) {
	var where []string
	var args []any

	if q != "" {
		pattern := "%" + escapeLike(q) + "%"
		where = append(where, `(name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	if category != "" {
		where = append(where, `categories LIKE ? ESCAPE '\'`)
		args = append(args, `%"`+escapeLike(category)+`"%`)
	}
	if afterFullName != "" {
		where = append(where, "(name > ? OR (name = ? AND full_name > ?))")
		args = append(args, afterName, afterName, afterFullName)
	}

	query := `SELECT full_name, namespace, name, description, latest_version,
		downloads, rating, is_deprecated, categories, icon_url FROM mod_packages`
	if len(where) > 0 {
		// where holds only the fixed clause literals above — never q, category or the
		// cursor, all of which travel exclusively through args as bound parameters.
		query += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: no request value reaches this string
	}
	query += " ORDER BY name ASC, full_name ASC LIMIT ?"
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

// escapeLike escapes a LIKE pattern's three special characters so a search term
// containing a literal "%" or "_" is matched literally, not as a wildcard. It does not
// escape a literal `"`, so a category name containing one would not reliably match its
// JSON-encoded form in the `categories` column — moot in practice, since Thunderstore's
// category names are a fixed, curated taxonomy (Mods, Libraries, QoL, ...) that has never
// contained one; revisit if that stops being true.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
