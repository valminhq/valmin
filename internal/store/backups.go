package store

import (
	"context"
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

// CreateBackup records a finished archive. It is written from data already in memory, so it
// is safe inside a job's Finish transaction (12 §6).
func (db *DB) CreateBackup(ctx context.Context, b *Backup) error {
	if _, err := db.Writer.ExecContext(ctx, `
		INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.InstanceID, b.Path, b.SizeBytes, b.SHA256, b.WorldName, b.Trigger, b.Consistent, Now(),
	); err != nil {
		return fmt.Errorf("record backup for instance %s: %w", b.InstanceID, err)
	}
	return nil
}

// ListBackups returns an instance's archives, newest first.
func (db *DB) ListBackups(ctx context.Context, instanceID string) ([]Backup, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at
		FROM backups WHERE instance_id = ? ORDER BY created_at DESC`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list backups for instance %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []Backup{}
	for rows.Next() {
		var b Backup
		var createdAt string
		if err := rows.Scan(&b.ID, &b.InstanceID, &b.Path, &b.SizeBytes, &b.SHA256,
			&b.WorldName, &b.Trigger, &b.Consistent, &createdAt); err != nil {
			return nil, fmt.Errorf("scan backup: %w", err)
		}
		if b.CreatedAt, err = ParseTime(createdAt); err != nil {
			return nil, fmt.Errorf("backup created_at: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backups for instance %s: %w", instanceID, err)
	}
	return out, nil
}
