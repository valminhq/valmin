// Package storetest opens migrated temporary databases for tests.
package storetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// Open returns a migrated database in a temporary directory, closed when the test ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	db, _ := OpenFile(t)
	return db
}

// OpenFile is Open, also returning the database file's path for a test that acts on the file.
//
// Every database is a copy of one migrated once per test binary: under the race detector the
// migrations cost close to a second, and a copy is byte-identical to what they would produce.
func OpenFile(t testing.TB) (db *store.DB, path string) {
	t.Helper()
	image, err := migrated()
	if err != nil {
		t.Fatalf("migrate the template database: %v", err)
	}
	path = filepath.Join(t.TempDir(), "panel.db")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("write test database: %v", err)
	}
	db, err = store.Open(t.Context(), "sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

var migrated = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "valmin-storetest-")
	if err != nil {
		return nil, fmt.Errorf("template directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return image(context.Background(), filepath.Join(dir, "panel.db"))
})

// image migrates a new database at path, closes it and returns the file's bytes. The WAL is
// checkpointed and must be empty afterwards, so the main file alone is the whole database.
func image(ctx context.Context, path string) ([]byte, error) {
	db, err := store.Open(ctx, "sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := store.Migrate(ctx, db.Writer); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return nil, fmt.Errorf("%s-wal still holds %d bytes after the checkpoint", path, fi.Size())
	}
	// #nosec G304 -- path is the database this function just created in its own temp directory.
	image, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return image, nil
}
