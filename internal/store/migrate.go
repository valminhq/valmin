package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one forward-only step. There are no down migrations: the rollback story
// for a panel that owns world data is "restore the DB and keep the worlds", which works
// because worlds live outside the DB lifecycle (ADR-024, 02 §2.7).
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// ErrChecksumMismatch reports that an already-applied migration's bytes have changed.
var ErrChecksumMismatch = errors.New("migration checksum mismatch: applied history was edited")

// ErrSchemaNewer reports that the database holds migrations this build does not know.
var ErrSchemaNewer = errors.New("database was written by a newer valmind")

// MigrationsApplied reports whether the database is reachable and fully migrated, in one
// round trip: counting the applied rows needs the table the migrations create, so a
// missing schema and an unreachable database both surface here. It is the database half
// of 11 §10's readiness probe.
func (db *DB) MigrationsApplied(ctx context.Context) error {
	want, err := Migrations()
	if err != nil {
		return err
	}
	var got int
	if err := db.Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&got); err != nil {
		return fmt.Errorf("count applied migrations: %w", err)
	}
	if got != len(want) {
		return fmt.Errorf("%d of %d migrations applied", got, len(want))
	}
	return nil
}

// Migrations reads and orders the embedded migration files.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: want <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(version)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  v,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1; got %d at position %d",
				m.Version, i+1)
		}
	}
	return out, nil
}

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMP NOT NULL
)`

// Migrate applies every pending migration in a transaction and verifies that already
// applied ones still match their recorded checksum (ADR-024).
func Migrate(ctx context.Context, db *sql.DB) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return err
	}
	if newest := slices.Max(append(slices.Collect(maps.Keys(applied)), 0)); newest > len(migrations) {
		return fmt.Errorf("%w: it has migration %d applied and this build knows up to %d; "+
			"run that newer version or restore the pre-upgrade database copy",
			ErrSchemaNewer, newest, len(migrations))
	}

	for _, m := range migrations {
		if have, ok := applied[m.Version]; ok {
			if have != m.Checksum {
				return fmt.Errorf("%w: migration %d (%s) recorded %s but the file now hashes to %s",
					ErrChecksumMismatch, m.Version, m.Name, have, m.Checksum)
			}
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func appliedChecksums(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int]string{}
	for rows.Next() {
		var (
			version  int
			checksum string
		)
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// applyOne runs a migration and records it in the same transaction, so a failure part
// way through leaves neither the schema nor the ledger half-written.
func applyOne(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Checksum, Now())
	if err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", m.Version, err)
	}
	slog.InfoContext(ctx, "applied migration",
		slog.Int("version", m.Version), slog.String("name", m.Name))
	return nil
}

// snapshotKeep is how many pre-migration copies SnapshotBeforeMigrate leaves in its directory.
const snapshotKeep = 3

// SnapshotBeforeMigrate copies the database into dir when a migration is pending and returns
// the copy's path. It returns "" and writes nothing for a fresh database or one with nothing
// pending. Copies beyond the newest three are removed.
func SnapshotBeforeMigrate(ctx context.Context, db *sql.DB, dir string, now time.Time) (string, error) {
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tables); err != nil {
		return "", fmt.Errorf("find schema_migrations: %w", err)
	}
	if tables == 0 {
		return "", nil
	}
	applied, err := appliedChecksums(ctx, db)
	if err != nil || len(applied) == 0 {
		return "", err
	}
	migrations, err := Migrations()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(migrations, func(m Migration) bool {
		_, ok := applied[m.Version]
		return !ok
	})
	if i < 0 {
		return "", nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("panel-pre-%04d-%s.db",
		migrations[i].Version, now.UTC().Format("20060102T150405Z")))
	// VACUUM INTO accepts an empty existing file, so creating it first fixes the mode.
	//nolint:gosec // dir is the data root's backups directory
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("copy database to %s: %w", path, err)
	}
	for _, p := range []string{path, dir} {
		if err := syncPath(p); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("sync database copy: %w", err)
		}
	}
	if err := pruneSnapshots(dir); err != nil {
		return "", err
	}
	return path, nil
}

// syncPath flushes a file or directory to disk.
func syncPath(path string) error {
	f, err := os.Open(path) //nolint:gosec // path is the snapshot or its directory
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// pruneSnapshots keeps the newest snapshotKeep copies, ordered by the timestamp in the name.
func pruneSnapshots(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "panel-pre-*.db"))
	if err != nil {
		return fmt.Errorf("list database copies: %w", err)
	}
	stamp := func(p string) string { return p[strings.LastIndex(p, "-")+1:] }
	slices.SortFunc(paths, func(a, b string) int {
		return cmp.Or(strings.Compare(stamp(b), stamp(a)), strings.Compare(b, a))
	})
	for _, p := range paths[min(len(paths), snapshotKeep):] {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove old database copy %s: %w", p, err)
		}
	}
	return nil
}
