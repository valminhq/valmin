package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
		m.Version, m.Checksum, time.Now().UTC().Format(time.RFC3339Nano))
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
