package storetest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// TestACopyMatchesAFreshMigration asserts a copied database has the same header settings,
// schema and rows as one migrated in place, apart from when each migration was applied. A
// migration that seeds a per-database value fails here rather than giving every test the same one.
func TestACopyMatchesAFreshMigration(t *testing.T) {
	copied := Open(t)

	fresh, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	if err := store.Migrate(t.Context(), fresh.Writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, want := dump(t, copied.Reader), dump(t, fresh.Reader)
	if len(got) != len(want) {
		t.Fatalf("copy has %d entries, fresh has %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("copy  %s\nfresh %s", got[i], want[i])
		}
	}
}

// TestCopiesAreIndependent asserts a write to one test's database is not seen by another's.
func TestCopiesAreIndependent(t *testing.T) {
	a, b := Open(t), Open(t)
	if _, err := a.Writer.ExecContext(t.Context(),
		`INSERT INTO kv (key, value, updated_at) VALUES ('storetest', '"a"', ?)`, store.Now()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := b.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM kv WHERE key = 'storetest'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("the second database sees %d rows written to the first", n)
	}
}

// dump renders the header settings, the schema and every row, one sorted line each.
func dump(t *testing.T, db *sql.DB) []string {
	t.Helper()
	pragmas := []string{"page_size", "auto_vacuum", "user_version", "application_id", "encoding", "journal_mode"}
	out := make([]string, 0, len(pragmas))
	for _, p := range pragmas {
		var v string
		if err := db.QueryRowContext(t.Context(), "PRAGMA "+p).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", p, err)
		}
		out = append(out, "pragma "+p+"="+v)
	}

	out = append(out, tableRows(t, db, "sqlite_master",
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master`)...)
	for _, table := range tableNames(t, db) {
		query := "SELECT * FROM " + table
		if table == "schema_migrations" {
			query = "SELECT version, checksum FROM schema_migrations"
		}
		out = append(out, tableRows(t, db, table, query)...)
	}
	return out
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("list tables: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return names
}

func tableRows(t *testing.T, db *sql.DB, table, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, fmt.Sprintf("row %s %v", table, vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	slices.Sort(out)
	return out
}
