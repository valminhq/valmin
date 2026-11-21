package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ModpackCode is the last profile code uploaded for a server's player modpack. ManifestSHA256
// is the digest of the mod list the code carries.
type ModpackCode struct {
	InstanceID     string
	Code           string
	Registry       string
	ManifestSHA256 string
	CreatedAt      time.Time
}

// SaveModpackCode records c as the server's profile code, replacing the previous one.
func (db *DB) SaveModpackCode(ctx context.Context, c *ModpackCode) error {
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO modpack_codes (instance_id, code, registry, manifest_sha256, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (instance_id) DO UPDATE SET
			code = excluded.code,
			registry = excluded.registry,
			manifest_sha256 = excluded.manifest_sha256,
			created_at = excluded.created_at`,
		c.InstanceID, c.Code, c.Registry, c.ManifestSHA256, FormatTime(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("save profile code for instance %s: %w", c.InstanceID, err)
	}
	return nil
}

// ModpackCode returns the server's last profile code, or nil when none was uploaded.
func (db *DB) ModpackCode(ctx context.Context, instanceID string) (*ModpackCode, error) {
	c := ModpackCode{InstanceID: instanceID}
	var createdAt string
	err := db.Reader.QueryRowContext(ctx, `
		SELECT code, registry, manifest_sha256, created_at FROM modpack_codes WHERE instance_id = ?`,
		instanceID).Scan(&c.Code, &c.Registry, &c.ManifestSHA256, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read profile code for instance %s: %w", instanceID, err)
	}
	if c.CreatedAt, err = ParseTime(createdAt); err != nil {
		return nil, fmt.Errorf("profile code created_at: %w", err)
	}
	return &c, nil
}
