package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRemoteBusy        = errors.New("remote backup transfer is active")
	ErrBackupProtected   = errors.New("backup is protected by a remote copy")
	ErrRemoteUnavailable = errors.New("remote backup destination is disabled or unavailable")
)

type RemoteDestination struct {
	ID            string  `json:"id"`
	Kind          string  `json:"kind"`
	Enabled       bool    `json:"enabled"`
	Retired       bool    `json:"retired"`
	Endpoint      string  `json:"endpoint"`
	Username      string  `json:"username"`
	Credentials   string  `json:"-"`
	RemoteName    string  `json:"remote_name"`
	Folder        string  `json:"folder"`
	LastTestAt    *string `json:"last_test_at"`
	LastTestError string  `json:"last_test_error"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

const destinationColumns = `id, kind, enabled, retired, endpoint, username, credentials,
remote_name, folder, last_test_at, last_test_error, created_at, updated_at`

func scanDestination(row scanner) (*RemoteDestination, error) {
	var d RemoteDestination
	err := row.Scan(&d.ID, &d.Kind, &d.Enabled, &d.Retired, &d.Endpoint, &d.Username,
		&d.Credentials, &d.RemoteName, &d.Folder, &d.LastTestAt, &d.LastTestError, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read remote destination: %w", err)
	}
	return &d, nil
}

func (db *DB) RemoteDestination(ctx context.Context) (*RemoteDestination, error) {
	return scanDestination(db.Reader.QueryRowContext(ctx, `SELECT `+destinationColumns+`
 FROM remote_backup_destinations WHERE retired = FALSE`))
}

func (db *DB) RemoteDestinationByID(ctx context.Context, id string) (*RemoteDestination, error) {
	return scanDestination(db.Reader.QueryRowContext(ctx, `SELECT `+destinationColumns+`
 FROM remote_backup_destinations WHERE id = ?`, id))
}

func (db *DB) SaveRemoteDestination(ctx context.Context, d *RemoteDestination, audit *AuditEntry) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remote destination update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM job_locks WHERE lock_key = 'global:remote_backup')`).
		Scan(&busy); err != nil {
		return fmt.Errorf("check remote transfer: %w", err)
	}
	if busy {
		return ErrRemoteBusy
	}
	// Replacement never repoints an existing copy at a different storage account.
	if _, err := tx.ExecContext(ctx, `UPDATE remote_copies SET status = 'cancelled', updated_at = ?
 WHERE status IN ('pending','retry_wait') AND destination_id <> ?`, Now(), d.ID); err != nil {
		return fmt.Errorf("cancel retired destination copies: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_backup_destinations SET retired = TRUE, enabled = FALSE
 WHERE retired = FALSE AND id <> ?`, d.ID); err != nil {
		return fmt.Errorf("retire remote destination: %w", err)
	}
	now := Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO remote_backup_destinations
 (id,kind,enabled,endpoint,username,credentials,remote_name,folder,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, credentials=excluded.credentials,
 username=excluded.username, updated_at=excluded.updated_at,
 last_test_at=CASE WHEN credentials<>excluded.credentials THEN NULL ELSE last_test_at END,
 last_test_error=CASE WHEN credentials<>excluded.credentials THEN '' ELSE last_test_error END`,
		d.ID, d.Kind, d.Enabled, d.Endpoint, d.Username, d.Credentials, d.RemoteName, d.Folder, now, now)
	if err != nil {
		return fmt.Errorf("save remote destination: %w", err)
	}
	if audit != nil {
		if err := TxWriteAuditLog(ctx, tx, audit); err != nil {
			return fmt.Errorf("audit remote destination: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remote destination: %w", err)
	}
	return nil
}

func (db *DB) RecordRemoteTest(ctx context.Context, id, message string) error {
	return recordRemoteTest(ctx, db.Writer, id, message)
}

func TxRecordRemoteTest(ctx context.Context, tx *sql.Tx, id, message string) error {
	return recordRemoteTest(ctx, tx, id, message)
}

func recordRemoteTest(ctx context.Context, ex execer, id, message string) error {
	_, err := ex.ExecContext(ctx, `UPDATE remote_backup_destinations
 SET last_test_at = ?, last_test_error = ? WHERE id = ?`, Now(), message, id)
	if err != nil {
		return fmt.Errorf("record remote destination test: %w", err)
	}
	return nil
}

func (db *DB) UpdateRemotePolicy(ctx context.Context, id string, enabled bool, cold, hot, snapshots int) error {
	_, err := db.Writer.ExecContext(ctx, `UPDATE instances SET remote_backup_enabled=?,
 remote_keep_cold=?, remote_keep_hot=?, remote_keep_snapshots=?, updated_at=? WHERE id=?`,
		enabled, cold, hot, snapshots, Now(), id)
	if err != nil {
		return fmt.Errorf("update remote backup policy: %w", err)
	}
	return nil
}

func TxCheckRemoteProtection(ctx context.Context, tx *sql.Tx, instanceID, backupID string) error {
	var protected bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM remote_copies
 WHERE instance_id=? AND (?='' OR backup_id=?) AND
 (status='uploading' OR (status IN ('pending','retry_wait') AND deadline_at>?)))`,
		instanceID, backupID, backupID, FormatTime(time.Now())).Scan(&protected)
	if err != nil {
		return fmt.Errorf("check remote backup protection: %w", err)
	}
	if protected {
		return ErrBackupProtected
	}
	return nil
}

func (db *DB) RemoteBackupProtected(ctx context.Context, instanceID, backupID string) (bool, error) {
	var protected bool
	err := db.Reader.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM remote_copies
 WHERE instance_id=? AND backup_id=? AND
 (status='uploading' OR (status IN ('pending','retry_wait') AND deadline_at>?)))`,
		instanceID, backupID, FormatTime(time.Now())).Scan(&protected)
	if err != nil {
		return false, fmt.Errorf("check remote backup protection: %w", err)
	}
	return protected, nil
}
