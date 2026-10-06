// Package storetest opens migrated temporary databases for tests.
package storetest

import (
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// Open returns a migrated database in a temporary directory, closed when the test ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return db
}
