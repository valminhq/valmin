package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	sqlite "modernc.org/sqlite" // cgo-free driver, ADR-004
)

// DB holds the two pools 10 §4.3 requires. SQLite in WAL mode allows many readers and
// one writer; letting database/sql open eight writers produces SQLITE_BUSY under exactly
// the load this panel generates.
//
// Writes go through Writer, reads through Reader. Reader connections are opened
// query_only, so a write sent to the wrong pool fails loudly instead of silently
// competing for the write lock.
type DB struct {
	Writer *sql.DB
	Reader *sql.DB
}

// readerConns bounds the reader pool. Small: this is a single-host panel, and every
// connection is a separate SQLite handle.
const readerConns = 8

// pragmas is the set 10 §4.3 fixes, in DSN form for modernc.org/sqlite.
//
// foreign_keys is the one that bites: SQLite disables it by default, which makes every
// ON DELETE CASCADE in the schema inert and silent (ADR-024).
var pragmas = []string{
	"_pragma=journal_mode(WAL)",
	"_pragma=foreign_keys(1)",
	"_pragma=busy_timeout(5000)",
	"_pragma=synchronous(NORMAL)",
}

// expected is what the pragmas must read back as. 10 §4.3 requires asserting rather than
// trusting the DSN: a pragma spelled the other driver's way is silently ignored.
//
// Ordered rather than a map so the reported failure is the same one every time, with
// foreign_keys first because it is the one whose absence is silent.
var expected = []struct{ name, want string }{
	{"foreign_keys", "1"},
	{"journal_mode", "wal"},
	{"busy_timeout", "5000"},
	{"synchronous", "1"}, // NORMAL
}

// Open connects both pools, verifies the pragma set on every connection, and returns a
// DB ready for Migrate.
func Open(ctx context.Context, driver, dsn string) (*DB, error) {
	if driver != "sqlite" {
		return nil, fmt.Errorf("unsupported db.driver %q: only sqlite is wired at M1", driver)
	}

	writer, err := openPool(ctx, dsn, false, 1)
	if err != nil {
		return nil, fmt.Errorf("writer pool: %w", err)
	}
	reader, err := openPool(ctx, dsn, true, readerConns)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("reader pool: %w", err)
	}
	return &DB{Writer: writer, Reader: reader}, nil
}

func openPool(ctx context.Context, dsn string, readOnly bool, maxConns int) (*sql.DB, error) {
	params := pragmas
	if readOnly {
		params = append(append([]string{}, pragmas...), "_pragma=query_only(1)")
	}

	db, err := sql.Open("sqlite", withParams(dsn, params))
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)

	if err := assertPragmas(ctx, db, maxConns); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// withParams appends query parameters to a DSN that may or may not already have some.
func withParams(dsn string, params []string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(params, "&")
}

// assertPragmas reads every pragma back on every connection in the pool. Checking one
// connection would not prove the DSN applies to the connections opened later under load.
func assertPragmas(ctx context.Context, db *sql.DB, maxConns int) error {
	// Hold each connection open so the pool is forced to create a new one for the next
	// iteration, rather than handing back the one just verified.
	conns := make([]*sql.Conn, 0, maxConns)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	for i := range maxConns {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("open connection %d: %w", i+1, err)
		}
		conns = append(conns, conn)

		for _, p := range expected {
			var got string
			row := conn.QueryRowContext(ctx, "PRAGMA "+p.name)
			if err := row.Scan(&got); err != nil {
				return fmt.Errorf("read PRAGMA %s on connection %d: %w", p.name, i+1, err)
			}
			if !strings.EqualFold(got, p.want) {
				return fmt.Errorf(
					"PRAGMA %s is %q on connection %d, want %q: the DSN parameter was not "+
						"honoured, so this pragma is silently inactive (10 §4.3)",
					p.name, got, i+1, p.want)
			}
		}
	}
	return nil
}

// Close shuts both pools down.
func (db *DB) Close() error {
	return errors.Join(db.Reader.Close(), db.Writer.Close())
}

// scanner is the common ground between *sql.Row and *sql.Rows, so one decode function can
// serve both a single lookup and a list — used by the users and invites scanners.
type scanner interface{ Scan(dest ...any) error }

// sqliteConstraintUnique and sqliteConstraintPrimaryKey are SQLite's *extended* result
// codes for a duplicate key, measured against modernc.org/sqlite v1.57.0 on 30 Aug 2026
// rather than assumed: 2067 and 1555, not the base SQLITE_CONSTRAINT (19) the names would
// suggest. A table whose duplicate-key column is its PRIMARY KEY (job_locks.lock_key)
// reports the second code; every UNIQUE-column table reports the first.
const (
	sqliteConstraintUnique     = 2067
	sqliteConstraintPrimaryKey = 1555
)

// isUniqueViolation reports whether err is a duplicate-key failure — UNIQUE or PRIMARY
// KEY — so a caller can turn it into name_taken (11 §2.5) or a lock conflict (12 §4.3)
// instead of a bare 500.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqliteConstraintUnique || se.Code() == sqliteConstraintPrimaryKey
}
