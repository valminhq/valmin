package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type RemoteCopy struct {
	ID               string  `json:"id"`
	DestinationID    string  `json:"destination_id"`
	InstanceID       string  `json:"instance_id"`
	BackupID         string  `json:"backup_id"`
	InstanceName     string  `json:"instance_name"`
	WorldName        string  `json:"world_name"`
	Trigger          string  `json:"trigger"`
	Consistent       bool    `json:"consistent"`
	SizeBytes        int64   `json:"size_bytes"`
	SHA256           string  `json:"sha256"`
	SourcePath       string  `json:"-"`
	ArchiveCreatedAt string  `json:"archive_created_at"`
	Status           string  `json:"status"`
	Attempts         int     `json:"attempts"`
	NextAttemptAt    string  `json:"next_attempt_at"`
	DeadlineAt       string  `json:"deadline_at"`
	LastError        string  `json:"last_error"`
	CancelRequested  bool    `json:"cancel_requested"`
	JobID            *string `json:"job_id"`
	ObjectJSON       string  `json:"-"`
	ManifestJSON     string  `json:"-"`
	SucceededAt      *string `json:"succeeded_at"`
	CleanupPending   bool    `json:"cleanup_pending"`
	CleanupError     string  `json:"cleanup_error"`
	CleanupNextAt    string  `json:"-"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

const remoteCopyColumns = `id,destination_id,instance_id,backup_id,instance_name,world_name,
trigger,consistent,size_bytes,sha256,source_path,archive_created_at,status,attempts,
next_attempt_at,deadline_at,last_error,cancel_requested,job_id,object_json,manifest_json,
succeeded_at,cleanup_pending,cleanup_error,cleanup_next_at,created_at,updated_at`

func scanRemoteCopy(row scanner) (*RemoteCopy, error) {
	var c RemoteCopy
	err := row.Scan(&c.ID, &c.DestinationID, &c.InstanceID, &c.BackupID, &c.InstanceName, &c.WorldName,
		&c.Trigger, &c.Consistent, &c.SizeBytes, &c.SHA256, &c.SourcePath, &c.ArchiveCreatedAt,
		&c.Status, &c.Attempts, &c.NextAttemptAt, &c.DeadlineAt, &c.LastError, &c.CancelRequested,
		&c.JobID, &c.ObjectJSON, &c.ManifestJSON, &c.SucceededAt, &c.CleanupPending, &c.CleanupError,
		&c.CleanupNextAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read remote copy: %w", err)
	}
	return &c, nil
}

// enqueueRemoteCopy runs with the archive insert, so a crash cannot lose upload intent.
func enqueueRemoteCopy(ctx context.Context, ex execer, instanceID, backupID string, automatic bool) error {
	now := time.Now()
	_, err := ex.ExecContext(ctx, `INSERT INTO remote_copies
 (id,destination_id,instance_id,backup_id,instance_name,world_name,trigger,consistent,
 size_bytes,sha256,source_path,archive_created_at,status,next_attempt_at,deadline_at,
 cleanup_next_at,created_at,updated_at)
 SELECT ?,d.id,b.instance_id,b.id,i.name,b.world_name,b.trigger,b.consistent,
 b.size_bytes,b.sha256,b.path,b.created_at,'pending',?,?,?,?,?
 FROM backups b JOIN instances i ON i.id=b.instance_id
 CROSS JOIN remote_backup_destinations d
 WHERE b.id=? AND b.instance_id=? AND i.state <> 'deleting'
 AND d.enabled=TRUE AND d.retired=FALSE AND (?=FALSE OR i.remote_backup_enabled=TRUE)
 ON CONFLICT(destination_id,backup_id) DO NOTHING`,
		NewID(), FormatTime(now), FormatTime(now.Add(24*time.Hour)), FormatTime(now),
		FormatTime(now), FormatTime(now), backupID, instanceID, automatic)
	if err != nil {
		return fmt.Errorf("queue remote backup: %w", err)
	}
	return nil
}

func (db *DB) QueueRemoteCopy(
	ctx context.Context,
	instanceID, backupID string,
	audit *AuditEntry,
) (*RemoteCopy, error) {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin remote backup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := enqueueRemoteCopy(ctx, tx, instanceID, backupID, false); err != nil {
		return nil, err
	}
	c, err := scanRemoteCopy(tx.QueryRowContext(ctx, `SELECT `+remoteCopyColumns+` FROM remote_copies
 WHERE instance_id=? AND backup_id=? AND destination_id IN
 (SELECT id FROM remote_backup_destinations WHERE retired=FALSE AND enabled=TRUE)`, instanceID, backupID))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrRemoteUnavailable
	}
	if audit != nil {
		if err := TxWriteAuditLog(ctx, tx, audit); err != nil {
			return nil, fmt.Errorf("audit remote copy: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit remote copy: %w", err)
	}
	return c, nil
}

func (db *DB) RemoteCopyByID(ctx context.Context, instanceID, id string) (*RemoteCopy, error) {
	return scanRemoteCopy(db.Reader.QueryRowContext(ctx, `SELECT `+remoteCopyColumns+` FROM remote_copies
 WHERE id=? AND instance_id=?`, id, instanceID))
}

func (db *DB) remoteCopies(ctx context.Context, where string, args ...any) ([]RemoteCopy, error) {
	//nolint:gosec // Callers supply fixed SQL clauses; all external values are bound parameters.
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+remoteCopyColumns+` FROM remote_copies `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list remote copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []RemoteCopy{}
	for rows.Next() {
		c, err := scanRemoteCopy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list remote copies: %w", err)
	}
	return out, nil
}

func (db *DB) ListRemoteCopies(ctx context.Context, instanceID, before, id string, limit int) ([]RemoteCopy, error) {
	return db.remoteCopies(ctx, `WHERE instance_id=? AND (?='' OR created_at<? OR (created_at=? AND id<?))
 ORDER BY created_at DESC,id DESC LIMIT ?`, instanceID, before, before, before, id, limit)
}

func (db *DB) DueRemoteCopies(ctx context.Context, now time.Time) ([]RemoteCopy, error) {
	return db.remoteCopies(ctx, `WHERE status IN ('pending','retry_wait') AND next_attempt_at<=? AND deadline_at>?
 AND destination_id IN (SELECT id FROM remote_backup_destinations WHERE enabled=TRUE AND retired=FALSE)
 AND instance_id IN (SELECT id FROM instances WHERE state <> 'deleting')
 ORDER BY next_attempt_at,id LIMIT 1`, FormatTime(now), FormatTime(now))
}

func (db *DB) ReconcileRemoteCopies(ctx context.Context) error {
	_, err := db.Writer.ExecContext(ctx, `UPDATE remote_copies SET status=CASE
 WHEN cancel_requested=TRUE THEN 'cancelled'
 WHEN deadline_at<=? THEN 'failed' ELSE 'retry_wait' END,
 last_error='Previous transfer was interrupted.',updated_at=?
 WHERE status='uploading' AND NOT EXISTS
 (SELECT 1 FROM job_runs j WHERE j.id=remote_copies.job_id AND j.status IN ('queued','running'))`, Now(), Now())
	if err != nil {
		return fmt.Errorf("recover remote copies: %w", err)
	}
	_, err = db.Writer.ExecContext(ctx, `UPDATE remote_copies SET status='failed',
 last_error='Upload retry window expired or source archive is unavailable.',updated_at=?
 WHERE status IN ('pending','retry_wait') AND
 (deadline_at<=? OR instance_id NOT IN (SELECT id FROM instances WHERE state <> 'deleting')
 OR NOT EXISTS (SELECT 1 FROM backups b WHERE b.id=remote_copies.backup_id AND b.instance_id=remote_copies.instance_id))`, Now(), Now())
	if err != nil {
		return fmt.Errorf("expire remote copies: %w", err)
	}
	return nil
}

func TxClaimRemoteCopy(ctx context.Context, tx *sql.Tx, id string) error {
	result, err := tx.ExecContext(ctx, `UPDATE remote_copies SET status='uploading',attempts=attempts+1,
 job_id=(SELECT job_id FROM job_locks WHERE lock_key='global:remote_backup'),updated_at=?
 WHERE id=? AND status IN ('pending','retry_wait') AND cancel_requested=FALSE AND deadline_at>?
 AND destination_id IN (SELECT id FROM remote_backup_destinations WHERE enabled=TRUE AND retired=FALSE)
 AND EXISTS (SELECT 1 FROM backups b JOIN instances i ON i.id=b.instance_id
 WHERE b.id=remote_copies.backup_id AND i.state <> 'deleting')`, Now(), id, Now())
	if err != nil {
		return fmt.Errorf("claim remote copy: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim remote copy count: %w", err)
	}
	if n != 1 {
		return ErrRemoteUnavailable
	}
	return nil
}

func (db *DB) SaveRemoteObjects(ctx context.Context, id, object, manifest string) error {
	_, err := db.Writer.ExecContext(
		ctx,
		`UPDATE remote_copies SET object_json=?,manifest_json=? WHERE id=?`,
		object,
		manifest,
		id,
	)
	if err != nil {
		return fmt.Errorf("save remote object references: %w", err)
	}
	return nil
}

func TxFinishRemoteCopy(ctx context.Context, tx *sql.Tx, id, status, message, next string) error {
	_, err := tx.ExecContext(ctx, `UPDATE remote_copies SET status=?,last_error=?,next_attempt_at=?,
 succeeded_at=CASE WHEN ?='succeeded' THEN ? ELSE succeeded_at END,updated_at=? WHERE id=?`,
		status, message, next, status, Now(), Now(), id)
	if err != nil {
		return fmt.Errorf("finish remote copy: %w", err)
	}
	return nil
}

func (db *DB) CancelRemoteCopy(ctx context.Context, instanceID, id string, audit *AuditEntry) error {
	return db.changeRemoteCopy(ctx, instanceID, id, false, audit)
}

func (db *DB) RetryRemoteCopy(ctx context.Context, instanceID, id string, audit *AuditEntry) error {
	return db.changeRemoteCopy(ctx, instanceID, id, true, audit)
}

func (db *DB) changeRemoteCopy(ctx context.Context, instanceID, id string, retry bool, audit *AuditEntry) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remote copy change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE remote_copies SET cancel_requested=TRUE,
 status=CASE WHEN status='uploading' THEN status ELSE 'cancelled' END,updated_at=?
 WHERE id=? AND instance_id=? AND status IN ('pending','retry_wait','uploading')`
	args := []any{Now(), id, instanceID}
	if retry {
		query = `UPDATE remote_copies SET cancel_requested=FALSE,status='pending',attempts=0,
 next_attempt_at=?,deadline_at=?,last_error='',updated_at=? WHERE id=? AND instance_id=?
 AND status IN ('failed','cancelled') AND EXISTS (SELECT 1 FROM backups b JOIN instances i
 ON i.id=b.instance_id WHERE b.id=remote_copies.backup_id AND i.state <> 'deleting')
 AND destination_id IN (SELECT id FROM remote_backup_destinations WHERE retired=FALSE AND enabled=TRUE)`
		args = []any{Now(), FormatTime(time.Now().Add(24 * time.Hour)), Now(), id, instanceID}
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("change remote copy: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("change remote copy count: %w", err)
	}
	if n != 1 {
		return ErrRemoteUnavailable
	}
	if audit != nil {
		if err := TxWriteAuditLog(ctx, tx, audit); err != nil {
			return fmt.Errorf("audit remote copy change: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remote copy change: %w", err)
	}
	return nil
}
