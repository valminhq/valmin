package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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

const insertBackup = `
	INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateBackup records a finished archive.
func (db *DB) CreateBackup(ctx context.Context, b *Backup) error {
	return createBackup(ctx, db.Writer, b)
}

// TxCreateBackup records a finished archive inside a caller's transaction, so a job's
// catalogue row lands in the same commit as its state flip (12 §6). It writes data already
// in memory: the archive is written and verified before the transaction opens (C1).
func TxCreateBackup(ctx context.Context, tx *sql.Tx, b *Backup) error {
	return createBackup(ctx, tx, b)
}

func createBackup(ctx context.Context, ex execer, b *Backup) error {
	if _, err := ex.ExecContext(ctx, insertBackup,
		b.ID, b.InstanceID, b.Path, b.SizeBytes, b.SHA256, b.WorldName, b.Trigger, b.Consistent, Now(),
	); err != nil {
		return fmt.Errorf("record backup for instance %s: %w", b.InstanceID, err)
	}
	return nil
}

// backupColumns is the row, in the order scanBackup reads it.
const backupColumns = `id, instance_id, path, size_bytes, sha256, world_name, trigger,
	consistent, created_at`

// scanBackup reads one row in backupColumns order.
func scanBackup(s scanner) (Backup, error) {
	var b Backup
	var createdAt string
	if err := s.Scan(&b.ID, &b.InstanceID, &b.Path, &b.SizeBytes, &b.SHA256,
		&b.WorldName, &b.Trigger, &b.Consistent, &createdAt); err != nil {
		return Backup{}, fmt.Errorf("scan backup row: %w", err)
	}
	var err error
	if b.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Backup{}, fmt.Errorf("backup created_at: %w", err)
	}
	return b, nil
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

// DeleteBackup removes one catalogue row. Unlinking the archive is the caller's: this
// package never touches the filesystem (C1).
func (db *DB) DeleteBackup(ctx context.Context, instanceID, id string) error {
	if _, err := db.Writer.ExecContext(ctx,
		`DELETE FROM backups WHERE id = ? AND instance_id = ?`, id, instanceID); err != nil {
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
