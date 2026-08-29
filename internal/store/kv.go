package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KVGet decodes the JSON value stored under key into v and reports whether the key
// exists. A missing key is not an error.
func (db *DB) KVGet(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read kv %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("decode kv %q: %w", key, err)
	}
	return true, nil
}

// KVSet writes v under key as JSON, replacing any existing value.
func (db *DB) KVSet(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode kv %q: %w", key, err)
	}
	_, err = db.Writer.ExecContext(ctx, `
		INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write kv %q: %w", key, err)
	}
	return nil
}
