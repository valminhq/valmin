package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
)

// TestAStaleCatalogueIsRecreated asserts a catalogue file at another schema version, or one
// that is not a database at all, is replaced by an empty catalogue rather than refusing to start.
func TestAStaleCatalogueIsRecreated(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(t *testing.T, path string){
		"another schema version": func(t *testing.T, path string) {
			db, err := openPool(t.Context(), "file:"+path, false, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 99`); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
		},
		"not a database": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("this is not sqlite, not even close to it"), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(path + "-wal")
			_ = os.Remove(path + "-shm")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := CataloguePath(dir)
			first := openWithCatalogue(t, dir, path)
			if err := first.UpsertModPackages(t.Context(), []ModPackage{{
				FullName: "a-b", Source: source.Thunderstore, Namespace: "a", Name: "b", CategoriesJSON: "[]",
			}}, nil); err != nil {
				t.Fatal(err)
			}
			_ = first.Close()

			spoil(t, path)

			second := openWithCatalogue(t, dir, path)
			var version, rows int
			if err := second.Catalogue.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := second.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mod_packages`).
				Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if version != catalogueVersion || rows != 0 {
				t.Errorf("reopened catalogue = version %d with %d rows, want version %d and empty",
					version, rows, catalogueVersion)
			}
		})
	}
}

// openWithCatalogue opens a panel database in dir with its catalogue at path, closed when the
// test ends.
func openWithCatalogue(t *testing.T, dir, path string) *DB {
	t.Helper()
	db, err := Open(t.Context(), "sqlite", "file:"+filepath.Join(dir, "panel.db"), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestCompactIfSparseShrinksTheFileAfterADrop asserts a file left mostly free by a dropped
// table is rebuilt smaller, and a dense one is left alone.
func TestCompactIfSparseShrinksTheFileAfterADrop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openWithCatalogue(t, dir, CataloguePath(dir))
	ctx := t.Context()

	if compacted, err := CompactIfSparse(ctx, db.Writer); err != nil || compacted {
		t.Fatalf("CompactIfSparse on a dense file = %v, %v; want false", compacted, err)
	}
	exec(t, db.Writer, `CREATE TABLE filler (blob BLOB)`)
	exec(t, db.Writer, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO filler SELECT randomblob(4096) FROM n`)
	exec(t, db.Writer, `DROP TABLE filler`)
	pages := func() int64 {
		t.Helper()
		var n int64
		if err := db.Writer.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := pages()

	compacted, err := CompactIfSparse(ctx, db.Writer)
	if err != nil || !compacted {
		t.Fatalf("CompactIfSparse after the drop = %v, %v; want true", compacted, err)
	}
	if after := pages(); after*4 > before {
		t.Errorf("page_count %d after compacting, want well under %d", after, before)
	}
}

// TestMigrationCarriesTheCatalogueAcross asserts the upgrade keeps the catalogue: rows move to
// the catalogue file and replace what it held, a row older than its registry's last complete
// listing arrives unlisted, a registry with no complete listing marks nothing, the sync state
// moves with the rows, and the panel file keeps none of it.
func TestMigrationCarriesTheCatalogueAcross(t *testing.T) {
	t.Parallel()
	db := migrateTo(t, 46)
	ctx := t.Context()
	listed, pulled, started := "2026-11-18T21:08:51.000000000Z", "2026-11-13T00:09:22.000000000Z",
		"2026-11-18T21:08:46.000000000Z"
	for name, syncedAt := range map[string]string{"a-Listed": listed, "a-Pulled": pulled} {
		exec(t, db.Writer, `INSERT INTO main.mod_packages (full_name, source, namespace, name,
			categories, synced_at) VALUES (?, 'thunderstore', 'a', ?, '[]', ?)`, name, name, syncedAt)
		exec(t, db.Writer, `INSERT INTO main.mod_versions (full_name, source, version, dependencies,
			download_url, file_size) VALUES (?, 'thunderstore', '1.0.0', '[]', 'https://x/y.zip', 7)`, name)
	}
	exec(t, db.Writer, `INSERT INTO main.mod_packages (full_name, source, namespace, name,
		categories, synced_at) VALUES ('h-Unlisted', 'hexium', 'h', 'Unlisted', '[]', ?)`, pulled)
	for key, value := range map[string]string{
		"thunderstore_etag": `"tag"`, "thunderstore_listing_started_at": `"` + started + `"`,
	} {
		exec(t, db.Writer, `INSERT INTO main.kv (key, value, updated_at) VALUES (?, ?, ?)`, key, value, Now())
	}
	// What a later build synced before a rollback to this schema.
	exec(t, db.Catalogue, `INSERT INTO mod_packages (full_name, source, namespace, name) VALUES
		('a-Listed', 'thunderstore', 'a', 'Newer'), ('z-Newer', 'thunderstore', 'z', 'Newer')`)
	exec(t, db.Catalogue, `INSERT INTO catalogue_state (key, value, updated_at) VALUES
		('thunderstore_etag', '"newer"', ?), ('hexium_listing_completed_at', '"x"', ?)`, Now(), Now())

	applyVersion(t, db, 47)

	unlisted := map[string]string{}
	rows, err := db.Catalogue.QueryContext(ctx, `SELECT full_name, COALESCE(unlisted_at, '') FROM mod_packages`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, at string
		if err := rows.Scan(&name, &at); err != nil {
			t.Fatal(err)
		}
		unlisted[name] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(unlisted) != 3 || unlisted["a-Listed"] != "" || unlisted["a-Pulled"] != pulled ||
		unlisted["h-Unlisted"] != "" {
		t.Errorf("catalogue packages = %v, want a-Listed and h-Unlisted listed, a-Pulled unlisted at %s",
			unlisted, pulled)
	}
	var versions, state int
	if err := db.Catalogue.QueryRowContext(ctx, `SELECT COUNT(*) FROM mod_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	var etag string
	if err := db.Catalogue.QueryRowContext(ctx, `SELECT COUNT(*), MAX(CASE WHEN key = 'thunderstore_etag'
		THEN value END) FROM catalogue_state`).Scan(&state, &etag); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || state != 2 || etag != `"tag"` {
		t.Errorf("catalogue holds %d versions, %d state keys and ETag %s; want 2, 2 and the panel's \"tag\"",
			versions, state, etag)
	}
	var left int
	if err := db.Writer.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM main.sqlite_master WHERE name IN ('mod_packages', 'mod_versions')) +
		(SELECT COUNT(*) FROM main.kv WHERE key LIKE 'thunderstore%')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the panel file still holds %d catalogue tables or keys", left)
	}
}
