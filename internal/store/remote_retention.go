package store

import (
	"context"
	"database/sql"
	"fmt"
)

type RemoteSummary struct {
	DestinationID           *string `json:"destination_id"`
	Enabled                 bool    `json:"enabled"`
	LastSuccessAt           *string `json:"last_success_at"`
	LastArchiveAt           *string `json:"last_archive_at"`
	LastConsistentArchiveAt *string `json:"last_consistent_archive_at"`
	Pending                 int     `json:"pending"`
	Failed                  int     `json:"failed"`
	CleanupPending          int     `json:"cleanup_pending"`
}

func (db *DB) RemoteSummary(ctx context.Context, instanceID string) (RemoteSummary, error) {
	var s RemoteSummary
	d, err := db.RemoteDestination(ctx)
	if err != nil {
		return s, err
	}
	if d == nil {
		return s, nil
	}
	s.DestinationID = &d.ID
	s.Enabled = d.Enabled
	err = db.Reader.QueryRowContext(ctx, `SELECT
 MAX(succeeded_at),
 (SELECT archive_created_at FROM remote_copies recent
 WHERE recent.destination_id=? AND (?='' OR recent.instance_id=?) AND recent.succeeded_at IS NOT NULL
 ORDER BY recent.succeeded_at DESC,recent.id DESC LIMIT 1),
 MAX(CASE WHEN status='succeeded' AND consistent=TRUE AND cleanup_pending=FALSE THEN archive_created_at END),
 COALESCE(SUM(status IN ('pending','retry_wait','uploading')),0),
 COALESCE(SUM(status='failed'),0),COALESCE(SUM(cleanup_pending),0)
 FROM remote_copies WHERE destination_id=? AND (?='' OR instance_id=?)`, d.ID, instanceID, instanceID, d.ID, instanceID, instanceID).
		Scan(&s.LastSuccessAt, &s.LastArchiveAt, &s.LastConsistentArchiveAt, &s.Pending, &s.Failed, &s.CleanupPending)
	if err != nil {
		return s, fmt.Errorf("read remote backup summary: %w", err)
	}
	return s, nil
}

func (db *DB) RemoteRetentionCopies(ctx context.Context, instanceID, destinationID string) ([]RemoteCopy, error) {
	return db.remoteCopies(ctx, `WHERE instance_id=? AND destination_id=? AND status='succeeded'
 ORDER BY archive_created_at DESC,id DESC`, instanceID, destinationID)
}

// UnmarkRemoteCleanup keeps a copy that retention marked and then took back, such as after a
// count was raised, as long as it has not been removed yet.
func (db *DB) UnmarkRemoteCleanup(ctx context.Context, id string) error {
	_, err := db.Writer.ExecContext(ctx, `UPDATE remote_copies SET cleanup_pending=FALSE,cleanup_error=''
 WHERE id=? AND status='succeeded' AND cleanup_pending=TRUE`, id)
	if err != nil {
		return fmt.Errorf("unmark remote retention: %w", err)
	}
	return nil
}

// MarkAbandonedRemoteUploads marks for cleanup every failed or cancelled copy of an instance
// that recorded an uploaded archive, which retention would otherwise never count or remove.
func (db *DB) MarkAbandonedRemoteUploads(ctx context.Context, instanceID, destinationID string) error {
	_, err := db.Writer.ExecContext(ctx, `UPDATE remote_copies SET cleanup_pending=TRUE
 WHERE instance_id=? AND destination_id=? AND status IN ('failed','cancelled')
 AND object_json<>'{}' AND cleanup_pending=FALSE`, instanceID, destinationID)
	if err != nil {
		return fmt.Errorf("mark abandoned remote uploads: %w", err)
	}
	return nil
}

func (db *DB) MarkRemoteCleanup(ctx context.Context, id string) error {
	_, err := db.Writer.ExecContext(
		ctx,
		`UPDATE remote_copies SET cleanup_pending=TRUE WHERE id=? AND status='succeeded'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("mark remote retention: %w", err)
	}
	return nil
}

func (db *DB) DueRemoteCleanup(ctx context.Context) ([]RemoteCopy, error) {
	return db.remoteCopies(ctx, `WHERE cleanup_pending=TRUE AND cleanup_next_at<=?
 AND instance_id IN (SELECT id FROM instances WHERE state <> 'deleting')
 AND destination_id IN (SELECT id FROM remote_backup_destinations WHERE enabled=TRUE AND retired=FALSE)
 ORDER BY cleanup_next_at,id LIMIT 1`, Now())
}

func (db *DB) FinishRemoteCleanup(ctx context.Context, id, message, next string) error {
	return finishRemoteCleanup(ctx, db.Writer, id, message, next)
}

func TxFinishRemoteCleanup(ctx context.Context, tx *sql.Tx, id, message, next string) error {
	return finishRemoteCleanup(ctx, tx, id, message, next)
}

func finishRemoteCleanup(ctx context.Context, ex execer, id, message, next string) error {
	// A failed or cancelled copy keeps its status and loses only the archive it recorded.
	_, err := ex.ExecContext(ctx, `UPDATE remote_copies SET
 status=CASE WHEN ?='' AND status='succeeded' THEN 'pruned' ELSE status END,
 object_json=CASE WHEN ?='' AND status<>'succeeded' THEN '{}' ELSE object_json END,
 manifest_json=CASE WHEN ?='' AND status<>'succeeded' THEN '{}' ELSE manifest_json END,
 cleanup_pending=(?<>''),cleanup_error=?,cleanup_next_at=?,updated_at=? WHERE id=?`,
		message, message, message, message, message, next, Now(), id)
	if err != nil {
		return fmt.Errorf("finish remote retention: %w", err)
	}
	return nil
}

// TxCheckRemoteCleanup rechecks ownership after the instance deletion lock is acquired.
func TxCheckRemoteCleanup(ctx context.Context, tx *sql.Tx, id string) error {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM remote_copies c
 JOIN instances i ON i.id=c.instance_id
 JOIN remote_backup_destinations d ON d.id=c.destination_id
 WHERE c.id=? AND c.status IN ('succeeded','failed','cancelled') AND c.cleanup_pending=TRUE
 AND i.state<>'deleting' AND d.enabled=TRUE AND d.retired=FALSE)`, id).Scan(&eligible)
	if err != nil {
		return fmt.Errorf("check remote cleanup: %w", err)
	}
	if !eligible {
		return ErrRemoteUnavailable
	}
	return nil
}
