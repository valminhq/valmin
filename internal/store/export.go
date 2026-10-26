package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"slices"
)

// CopyTo writes a consistent, compacted copy of the database to path, mode 0600. It runs on a
// reader connection, so writes carry on while it copies. path must not exist.
func (db *DB) CopyTo(ctx context.Context, path string) error {
	// VACUUM INTO accepts an empty existing file, so creating it first fixes the mode.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // panel-built path
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	conn, err := db.Reader.Conn(ctx)
	if err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// VACUUM INTO writes the copy, which query_only forbids. The connection is discarded after,
	// so the pool never hands out one that may write.
	defer func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA query_only = 0`); err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("copy database to %s: %w", path, err)
	}
	if err := syncPath(path); err != nil {
		return fmt.Errorf("sync database copy: %w", err)
	}
	return nil
}

// SchemaVersion is the number of migrations this build knows, which is the version a database
// it has migrated is at.
func SchemaVersion() (int, error) {
	migrations, err := Migrations()
	if err != nil {
		return 0, err
	}
	return len(migrations), nil
}

// BackupFile is one catalogued world archive and the path it is recorded at.
type BackupFile struct {
	InstanceID string
	ID         string
	Path       string
}

// BackupFiles lists every catalogued world archive.
func (db *DB) BackupFiles(ctx context.Context) ([]BackupFile, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT instance_id, id, path FROM backups ORDER BY instance_id, id`)
	if err != nil {
		return nil, fmt.Errorf("list backup files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BackupFile
	for rows.Next() {
		var b BackupFile
		if err := rows.Scan(&b.InstanceID, &b.ID, &b.Path); err != nil {
			return nil, fmt.Errorf("scan backup file: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read backup files: %w", err)
	}
	return out, nil
}

// PrepareRestored readies a database unpacked from an installation export, in one
// transaction: every provisioned instance outside error is marked stopped, since its container
// stayed on the old host, each instance named in builds gets that build recorded, the export
// job the copy caught running is closed as succeeded, and the catalogue rows of the archives
// the export left out are dropped, after unlinking the saved setups that named them.
func (db *DB) PrepareRestored(ctx context.Context, builds map[string]string, missing []BackupFile) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prepare restored database: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := Now()
	if _, err := tx.ExecContext(ctx, `UPDATE instances SET state = 'stopped', updated_at = ?
		WHERE state NOT IN ('created', 'error')`, now); err != nil {
		return fmt.Errorf("mark restored servers stopped: %w", err)
	}
	for id, build := range builds {
		if _, err := tx.ExecContext(ctx, `UPDATE instances SET game_build_id = ?, updated_at = ? WHERE id = ?`,
			build, now, id); err != nil {
			return fmt.Errorf("record the build of server %s: %w", id, err)
		}
	}
	// The copy was taken while the export that made it was running, and that export finished.
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_locks WHERE job_id IN
		(SELECT id FROM job_runs WHERE kind = 'export' AND status = 'running')`); err != nil {
		return fmt.Errorf("release the export's locks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET status = 'succeeded', progress = 100,
		lease_owner = NULL, lease_until = NULL, finished_at = ?
		WHERE kind = 'export' AND status = 'running'`, Now()); err != nil {
		return fmt.Errorf("finish the export job: %w", err)
	}
	for b := range slices.Values(missing) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE saved_setups SET backup_id = NULL WHERE instance_id = ? AND backup_id = ?`,
			b.InstanceID, b.ID); err != nil {
			return fmt.Errorf("unlink setups from backup %s: %w", b.ID, err)
		}
		if err := deleteBackupRow(ctx, tx, b.InstanceID, b.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prepare restored database: commit: %w", err)
	}
	return nil
}
