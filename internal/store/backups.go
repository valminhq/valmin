package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/backup"
)

// Backup is one row of the archive catalogue (04 §2). A row exists only once the archive
// has been renamed into place (12 §9.4), so a catalogue entry always names a complete file —
// which is what lets a restore trust the row without re-verifying the archive first.
type Backup struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id"`
	Path       string    `json:"-"` // 11 §8.3: never a raw path in a response
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	WorldName  string    `json:"world_name"`
	Trigger    string    `json:"trigger"`
	Consistent bool      `json:"consistent"`
	CreatedAt  time.Time `json:"created_at"`
	// WorldBytes is the size of the world inside the archive, nil when it was never measured.
	WorldBytes *int64 `json:"world_bytes"`
	// WorldAttention is what the world check asked the operator to look at. Empty when the world
	// passed, was acknowledged, or was never checked.
	WorldAttention []backup.Attention `json:"world_attention"`
}

// Backup trigger values, matching the CHECK constraint in migration 0001.
const (
	TriggerManual     = "manual"
	TriggerScheduled  = "scheduled"
	TriggerPreUpdate  = "pre_update"
	TriggerPreRestore = "pre_restore"
	// TriggerPreImport is 03 §4.1 rule 6's snapshot: the world that was there before an
	// import replaced it.
	TriggerPreImport = "pre_import"
)

// SnapshotTrigger reports whether an archive with this trigger is a snapshot: the copy a
// restore, import or update takes of the world it is about to replace. Retention counts
// snapshots apart from the operator's own archives.
func SnapshotTrigger(trigger string) bool {
	return trigger == TriggerPreUpdate || trigger == TriggerPreRestore || trigger == TriggerPreImport
}

// snapshotTriggers is SnapshotTrigger's set as an SQL list.
const snapshotTriggers = `('pre_update','pre_restore','pre_import')`

const insertBackup = `
	INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent,
		world_bytes, world_attention, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateBackup records a finished archive.
func (db *DB) CreateBackup(ctx context.Context, b *Backup) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin backup catalogue insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := createBackup(ctx, tx, b); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backup catalogue insert: %w", err)
	}
	return nil
}

// TxCreateBackup records a finished archive inside a caller's transaction, so a job's
// catalogue row lands in the same commit as its state flip (12 §6). It writes data already
// in memory: the archive is written and verified before the transaction opens (C1).
func TxCreateBackup(ctx context.Context, tx *sql.Tx, b *Backup) error {
	return createBackup(ctx, tx, b)
}

func createBackup(ctx context.Context, ex execer, b *Backup) error {
	attention, err := encodeAttention(b.WorldAttention)
	if err != nil {
		return fmt.Errorf("record backup for instance %s: %w", b.InstanceID, err)
	}
	if _, err := ex.ExecContext(ctx, insertBackup,
		b.ID, b.InstanceID, b.Path, b.SizeBytes, b.SHA256, b.WorldName, b.Trigger, b.Consistent,
		b.WorldBytes, attention, Now(),
	); err != nil {
		return fmt.Errorf("record backup for instance %s: %w", b.InstanceID, err)
	}
	return enqueueRemoteCopy(ctx, ex, b.InstanceID, b.ID, true)
}

// backupColumns is the row, in the order scanBackup reads it.
const backupColumns = `id, instance_id, path, size_bytes, sha256, world_name, trigger,
	consistent, created_at, world_bytes, world_attention`

// scanBackup reads one row in backupColumns order.
func scanBackup(s scanner) (Backup, error) {
	var b Backup
	var createdAt string
	var worldBytes sql.NullInt64
	var attention sql.NullString
	if err := s.Scan(&b.ID, &b.InstanceID, &b.Path, &b.SizeBytes, &b.SHA256,
		&b.WorldName, &b.Trigger, &b.Consistent, &createdAt, &worldBytes, &attention); err != nil {
		return Backup{}, fmt.Errorf("scan backup row: %w", err)
	}
	var err error
	if b.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Backup{}, fmt.Errorf("backup created_at: %w", err)
	}
	if worldBytes.Valid {
		b.WorldBytes = &worldBytes.Int64
	}
	if attention.Valid {
		if err := json.Unmarshal([]byte(attention.String), &b.WorldAttention); err != nil {
			return Backup{}, fmt.Errorf("backup %s world_attention: %w", b.ID, err)
		}
	}
	return b, nil
}

// encodeAttention renders findings for the world_attention column, NULL for none.
func encodeAttention(found []backup.Attention) (any, error) {
	if len(found) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(found)
	if err != nil {
		return nil, fmt.Errorf("encode world attention: %w", err)
	}
	return string(raw), nil
}

// WorldBaseline is the archive a new one's world size is compared against: the newest of
// instanceID's archives of worldName that was measured, passed the world check and is not a
// snapshot, taken after the instance's newest snapshot. A snapshot marks the panel changing the
// world on purpose, so the comparison starts again after one. ok is false when there is none.
func (db *DB) WorldBaseline(
	ctx context.Context, instanceID, worldName string,
) (bytes int64, at time.Time, ok bool, err error) {
	var createdAt string
	err = db.Reader.QueryRowContext(ctx, `SELECT world_bytes, created_at FROM backups
		WHERE instance_id = ? AND world_name = ? AND world_bytes IS NOT NULL
		AND world_attention IS NULL AND trigger NOT IN `+snapshotTriggers+`
		AND created_at > COALESCE((SELECT MAX(created_at) FROM backups
			WHERE instance_id = ? AND trigger IN `+snapshotTriggers+`), '')
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		instanceID, worldName, instanceID).Scan(&bytes, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read the world baseline of instance %s: %w", instanceID, err)
	}
	if at, err = ParseTime(createdAt); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("world baseline created_at: %w", err)
	}
	return bytes, at, true, nil
}

// AcknowledgeWorld clears the world check's findings on one archive and its remote copies,
// which makes it the size baseline later archives are compared against, and records audit
// with it when given. It
// reports whether the archive exists.
func (db *DB) AcknowledgeWorld(ctx context.Context, instanceID, id string, audit *AuditEntry) (bool, error) {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin world acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE backups SET world_attention = NULL WHERE id = ? AND instance_id = ?`, id, instanceID)
	if err != nil {
		return false, fmt.Errorf("acknowledge the world of backup %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acknowledge the world of backup %s: %w", id, err)
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_copies SET world_flagged = FALSE
		WHERE backup_id = ? AND instance_id = ?`, id, instanceID); err != nil {
		return false, fmt.Errorf("acknowledge the remote copies of backup %s: %w", id, err)
	}
	if audit != nil {
		if err := writeAuditLog(ctx, tx, audit, time.Now().UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit world acknowledgement: %w", err)
	}
	return true, nil
}

// FlaggedLatest is, for each instance whose newest archive other than a snapshot was flagged by
// the world check, that archive. An instance whose newest archive passed is absent.
func (db *DB) FlaggedLatest(ctx context.Context) (map[string]Backup, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM backups b
		WHERE world_attention IS NOT NULL AND trigger NOT IN %s
		AND NOT EXISTS (SELECT 1 FROM backups n WHERE n.instance_id = b.instance_id
			AND n.trigger NOT IN %s
			AND (n.created_at > b.created_at OR (n.created_at = b.created_at AND n.id > b.id)))`,
		backupColumns, snapshotTriggers, snapshotTriggers))
	if err != nil {
		return nil, fmt.Errorf("read flagged backups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out[b.InstanceID] = b
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read flagged backups: %w", err)
	}
	return out, nil
}

// BackupByID reads one archive belonging to instanceID, or (nil, nil) when there is no such
// row, which the handler answers as 404 (D2, ADR-038). Scoping by instance is what keeps an
// id from another instance indistinguishable from one that never existed.
func (db *DB) BackupByID(ctx context.Context, instanceID, id string) (*Backup, error) {
	row := db.Reader.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT %s FROM backups WHERE id = ? AND instance_id = ?`, backupColumns), id, instanceID)
	b, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up backup %s: %w", id, err)
	}
	return &b, nil
}

const deleteBackup = `DELETE FROM backups WHERE id = ? AND instance_id = ?`

// DeleteBackup removes one catalogue row. Unlinking the archive is the caller's: this
// package never touches the filesystem (C1).
func (db *DB) DeleteBackup(ctx context.Context, instanceID, id string) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin backup deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := TxDeleteBackup(ctx, tx, instanceID, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backup deletion: %w", err)
	}
	return nil
}

// TxDeleteBackup removes one catalogue row inside a caller's transaction, so a job's prune
// commits with the rest of its Finish. The caller unlinks the archive after the commit, and
// only once the row is gone.
func TxDeleteBackup(ctx context.Context, tx *sql.Tx, instanceID, id string) error {
	if err := TxCheckRemoteProtection(ctx, tx, instanceID, id); err != nil {
		return err
	}
	return deleteBackupRow(ctx, tx, instanceID, id)
}

func deleteBackupRow(ctx context.Context, ex execer, instanceID, id string) error {
	if _, err := ex.ExecContext(ctx, deleteBackup, id, instanceID); err != nil {
		return fmt.Errorf("delete backup %s: %w", id, err)
	}
	return nil
}

// ListBackups returns an instance's archives, newest first, one keyset page at a time
// (ADR-035): created_at with id breaking the tie, since two archives can share a second.
func (db *DB) ListBackups(
	ctx context.Context, instanceID, beforeCreatedAt, beforeID string, limit int,
) ([]Backup, error) {
	where := "instance_id = ?"
	args := []any{instanceID}
	if beforeCreatedAt != "" {
		where += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	}
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM backups WHERE %s ORDER BY created_at DESC, id DESC LIMIT ?`,
		backupColumns, where), args...)
	if err != nil {
		return nil, fmt.Errorf("list backups for instance %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backups for instance %s: %w", instanceID, err)
	}
	return out, nil
}

// LastBackupTimes returns each instance's newest consistent archive time. A hot copy is left
// out: it is best-effort, so it must not stand in for the backup a schedule owes. An instance
// absent from the map has no consistent archive on record.
func (db *DB) LastBackupTimes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := db.Reader.QueryContext(ctx,
		`SELECT instance_id, MAX(created_at) FROM backups WHERE consistent GROUP BY instance_id`)
	if err != nil {
		return nil, fmt.Errorf("read last backup times: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]time.Time{}
	for rows.Next() {
		var instanceID, at string
		if err := rows.Scan(&instanceID, &at); err != nil {
			return nil, fmt.Errorf("scan last backup time: %w", err)
		}
		t, err := ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("parse last backup time: %w", err)
		}
		out[instanceID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read last backup times: %w", err)
	}
	return out, nil
}
