package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigrateRefusesANewerSchema asserts that an applied version this build does not know
// stops Migrate before it applies anything, and that the error names both versions.
func TestMigrateRefusesANewerSchema(t *testing.T) {
	db := open(t)
	want, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	last := want[len(want)-1].Version
	unknown := last + 5

	exec(t, db.Writer, `DELETE FROM schema_migrations WHERE version = ?`, last)
	exec(t, db.Writer,
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, 'x', ?)`,
		unknown, Now())

	err = Migrate(t.Context(), db.Writer)
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Migrate error = %v, want ErrSchemaNewer", err)
	}
	for _, s := range []string{strconv.Itoa(unknown), strconv.Itoa(last), "restore"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}

	var n int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, last).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("migration %d was applied despite the refusal", last)
	}
}

// withPending returns a migrated database whose newest migration is recorded as unapplied.
func withPending(t *testing.T) (db *DB, pending int) {
	t.Helper()
	db = open(t)
	want, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	pending = want[len(want)-1].Version
	exec(t, db.Writer, `DELETE FROM schema_migrations WHERE version = ?`, pending)
	return db, pending
}

// TestSnapshotBeforeMigrate asserts the copy is written with mode 0600 through the writer
// while a reader holds an open read transaction, passes integrity_check and holds the data.
func TestSnapshotBeforeMigrate(t *testing.T) {
	db, pending := withPending(t)
	exec(t, db.Writer, `INSERT INTO kv (key, value, updated_at) VALUES ('probe', '"kept"', ?)`, Now())

	tx, err := db.Reader.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(t.Context(), `SELECT key FROM kv`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("reader saw no rows: %v", rows.Err())
	}

	dir := filepath.Join(t.TempDir(), "backups")
	now := time.Date(2026, 9, 27, 10, 15, 0, 0, time.UTC)
	path, err := SnapshotBeforeMigrate(t.Context(), db.Writer, dir, now)
	if err != nil {
		t.Fatalf("SnapshotBeforeMigrate: %v", err)
	}
	wantName := "panel-pre-" + padVersion(pending) + "-20260927T101500Z.db"
	if filepath.Base(path) != wantName {
		t.Errorf("snapshot is %s, want %s", filepath.Base(path), wantName)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot mode is %#o, want 0600", perm)
	}

	cp, err := Open(t.Context(), "sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer func() { _ = cp.Close() }()
	var check string
	if err := cp.Reader.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatal(err)
	}
	if check != "ok" {
		t.Errorf("integrity_check = %q", check)
	}
	var got string
	if _, err := cp.KVGet(t.Context(), "probe", &got); err != nil || got != "kept" {
		t.Errorf("snapshot kv probe = %q, %v", got, err)
	}
}

func padVersion(v int) string {
	s := strconv.Itoa(v)
	return strings.Repeat("0", 4-len(s)) + s
}

// TestSnapshotBeforeMigrateSkips asserts no file is written for a fresh database or one with
// nothing pending.
func TestSnapshotBeforeMigrateSkips(t *testing.T) {
	fresh := func(t *testing.T) *DB {
		db, err := Open(t.Context(), "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	tests := map[string]func(t *testing.T) *DB{
		"no ledger": fresh,
		"empty ledger": func(t *testing.T) *DB {
			db := fresh(t)
			exec(t, db.Writer, createSchemaMigrations)
			return db
		},
		"nothing pending": open,
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			db := setup(t)
			dir := filepath.Join(t.TempDir(), "backups")
			path, err := SnapshotBeforeMigrate(t.Context(), db.Writer, dir, time.Now())
			if err != nil {
				t.Fatalf("SnapshotBeforeMigrate: %v", err)
			}
			if path != "" {
				t.Errorf("snapshot path = %q, want none", path)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("backups dir exists: %v", err)
			}
		})
	}
}

// TestSnapshotBeforeMigrateKeepsThree asserts only the three newest copies survive and that
// other files in the directory are left alone.
func TestSnapshotBeforeMigrateKeepsThree(t *testing.T) {
	db, _ := withPending(t)
	dir := t.TempDir()
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	made := make([]string, 0, 5)
	for i := range 5 {
		path, err := SnapshotBeforeMigrate(t.Context(), db.Writer, dir, base.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		made = append(made, filepath.Base(path))
	}

	left, err := filepath.Glob(filepath.Join(dir, "panel-pre-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(left))
	for _, p := range left {
		names = append(names, filepath.Base(p))
	}
	slices.Sort(names)
	if !slices.Equal(names, made[2:]) {
		t.Errorf("kept %v, want %v", names, made[2:])
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
}
