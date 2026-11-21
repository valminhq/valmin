package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/source"
)

//go:embed catalogue.sql
var catalogueSchema string

// catalogueVersion is the schema catalogue.sql creates. A file at any other version is deleted
// and created again, which costs one sync.
const catalogueVersion = 1

// CataloguePath is where the mod catalogue lives under data.root.
func CataloguePath(dataRoot string) string {
	return filepath.Join(dataRoot, "cache", "catalogue.db")
}

// errStaleCatalogue marks a catalogue file this build cannot use: another schema version, or
// not a sound SQLite database.
var errStaleCatalogue = errors.New("stale catalogue")

// openCatalogue opens the catalogue's single writer connection at path. A file that is corrupt,
// not a database, or at another schema version is a stale cache: it is deleted and created
// empty. Any other failure is returned, so a transient error never costs the catalogue.
func openCatalogue(ctx context.Context, path string) (*sql.DB, error) {
	if err := fsutil.MkdirAllExact(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("catalogue directory: %w", err)
	}
	db, err := openCatalogueFile(ctx, path)
	if err == nil || !errors.Is(err, errStaleCatalogue) {
		return db, err
	}
	slog.WarnContext(ctx, "recreating the mod catalogue", slog.String("path", path), slog.Any("error", err))
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale catalogue: %w", err)
		}
	}
	return openCatalogueFile(ctx, path)
}

// openCatalogueFile opens path and creates the schema when the file is new. An error wrapping
// errStaleCatalogue means the file itself is unusable.
func openCatalogueFile(ctx context.Context, path string) (*sql.DB, error) {
	db, err := openPool(ctx, "file:"+path, false, 1, "")
	if err != nil {
		return nil, staleIfDamaged(err)
	}
	if err := ensureCatalogueSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, staleIfDamaged(err)
	}
	return db, nil
}

// staleIfDamaged marks err stale when SQLite reports the file corrupt or not a database.
func staleIfDamaged(err error) error {
	const sqliteCorrupt, sqliteNotADB = 11, 26
	var se *sqlite.Error
	if errors.As(err, &se) && (se.Code()&0xff == sqliteCorrupt || se.Code()&0xff == sqliteNotADB) {
		return fmt.Errorf("%w: %w", errStaleCatalogue, err)
	}
	return err
}

func ensureCatalogueSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read catalogue version: %w", err)
	}
	switch version {
	case catalogueVersion:
		return nil
	case 0:
		return createCatalogueSchema(ctx, db)
	default:
		return fmt.Errorf("%w: schema version %d, want %d", errStaleCatalogue, version, catalogueVersion)
	}
}

// createCatalogueSchema creates the tables and stamps the version in one transaction, so a
// crash leaves either an empty file or a complete one.
func createCatalogueSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create catalogue schema: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, catalogueSchema); err != nil {
		return fmt.Errorf("create catalogue schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, catalogueVersion)); err != nil {
		return fmt.Errorf("stamp catalogue version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create catalogue schema: commit: %w", err)
	}
	return nil
}

// attachingConnector opens panel connections with the catalogue attached, so a query names the
// catalogue's tables unqualified, joins included, exactly as it names the panel's own.
type attachingConnector struct {
	driver *sqlite.Driver
	dsn    string
}

func newAttachingConnector(dsn, cataloguePath string) attachingConnector {
	// A driver of its own, so the hook reaches only these connections.
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), `ATTACH DATABASE ? AS catalogue`,
			[]driver.NamedValue{{Ordinal: 1, Value: cataloguePath}})
		if err != nil {
			return fmt.Errorf("attach the mod catalogue: %w", err)
		}
		return nil
	})
	return attachingConnector{driver: d, dsn: dsn}
}

func (c attachingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", c.dsn, err)
	}
	return conn, nil
}

func (c attachingConnector) Driver() driver.Driver { return c.driver }

// CatalogueStateGet decodes the catalogue state value stored under key into v, reporting
// whether the key exists.
func (db *DB) CatalogueStateGet(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := db.Reader.QueryRowContext(ctx, `SELECT value FROM catalogue_state WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read catalogue state %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("decode catalogue state %q: %w", key, err)
	}
	return true, nil
}

// CatalogueStateSet stores v as JSON under key, through the catalogue's own writer.
func (db *DB) CatalogueStateSet(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode catalogue state %q: %w", key, err)
	}
	if _, err := db.Catalogue.ExecContext(ctx, `
		INSERT INTO catalogue_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), Now()); err != nil {
		return fmt.Errorf("write catalogue state %q: %w", key, err)
	}
	return nil
}

// MarkUnlisted stamps every package of src that a complete listing, whose names are listed,
// did not include. A package already marked keeps its first stamp; the next upsert of a name
// clears it.
func (db *DB) MarkUnlisted(ctx context.Context, src source.Source, listed map[string]bool, now time.Time) error {
	tx, err := db.Catalogue.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mark unlisted packages: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	missing, err := listedMissing(ctx, tx, src, listed)
	if err != nil {
		return err
	}
	stamp := FormatTime(now)
	for _, name := range missing {
		if _, err := tx.ExecContext(ctx,
			`UPDATE mod_packages SET unlisted_at = ? WHERE full_name = ? AND source = ?`,
			stamp, name, src.String()); err != nil {
			return fmt.Errorf("mark %s unlisted: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark unlisted packages: commit: %w", err)
	}
	return nil
}

// listedMissing returns the names src still lists in the catalogue that listed omits.
func listedMissing(ctx context.Context, tx *sql.Tx, src source.Source, listed map[string]bool) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT full_name FROM mod_packages WHERE source = ? AND unlisted_at IS NULL`, src.String())
	if err != nil {
		return nil, fmt.Errorf("read listed packages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan listed package: %w", err)
		}
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read listed packages: %w", err)
	}
	return missing, nil
}
