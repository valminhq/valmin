package store

import (
	"context"
	"fmt"
)

// encryptedColumn is one column 10 §3 seals. The registry is closed and each entry carries
// its own statements rather than interpolating a table name: a column missing from here is
// a secret the rotation sweep leaves on an old generation with nothing to notice it.
type encryptedColumn struct {
	table  string
	column string
	// purpose is a crypto.Purpose by value rather than by type: crypto's own tests reach for
	// this package, so the dependency runs one way only. A purpose that does not match the
	// constant it names fails the first decrypt rather than corrupting anything.
	purpose string
	// stale selects rows whose envelope names some other generation, oldest id first.
	stale string
	// replace rewrites one row only if it still holds the envelope the sweep read.
	replace string
}

var encryptedColumns = []encryptedColumn{
	{
		table: "instances", column: "password", purpose: "instance-password",
		stale: `SELECT id, password FROM instances
		        WHERE password <> '' AND password NOT LIKE ? ORDER BY id LIMIT ?`,
		replace: `UPDATE instances SET password = ? WHERE id = ? AND password = ?`,
	},
	{
		table: "instances", column: "rcon_password", purpose: "rcon-password",
		stale: `SELECT id, rcon_password FROM instances
		        WHERE rcon_password IS NOT NULL AND rcon_password <> ''
		          AND rcon_password NOT LIKE ? ORDER BY id LIMIT ?`,
		replace: `UPDATE instances SET rcon_password = ? WHERE id = ? AND rcon_password = ?`,
	},
	{
		// Swept whether or not enrolment exists: an operator who rotates must not be told to
		// enrol first, and a nonempty field is a secret regardless of which build wrote it.
		table: "users", column: "totp_secret", purpose: "totp-secret",
		stale: `SELECT id, totp_secret FROM users
		        WHERE totp_secret IS NOT NULL AND totp_secret <> ''
		          AND totp_secret NOT LIKE ? ORDER BY id LIMIT ?`,
		replace: `UPDATE users SET totp_secret = ? WHERE id = ? AND totp_secret = ?`,
	},
	{
		// A destination URL is a bearer credential (05 M6): whoever holds it can post to
		// that channel, so it rotates with the passwords rather than sitting outside the
		// sweep as configuration.
		table: "webhooks", column: "url", purpose: "webhook-url",
		stale: `SELECT id, url FROM webhooks
		        WHERE url <> '' AND url NOT LIKE ? ORDER BY id LIMIT ?`,
		replace: `UPDATE webhooks SET url = ? WHERE id = ? AND url = ?`,
	},
}

// StaleSecret is one encrypted value sealed under a generation that is no longer the write
// key.
type StaleSecret struct {
	Table    string
	Column   string
	RowID    string
	Purpose  string
	Envelope string
}

// CountStaleSecrets reports how many encrypted values are not yet on keyID.
func (db *DB) CountStaleSecrets(ctx context.Context, keyID string) (int, error) {
	total := 0
	for i := range encryptedColumns {
		found, err := db.staleIn(ctx, &encryptedColumns[i], keyID, 0)
		if err != nil {
			return 0, err
		}
		total += len(found)
	}
	return total, nil
}

// ListStaleSecrets returns up to limit encrypted values sealed under a generation other than
// keyID. The envelope names its own generation (10 §3.2), so what a sweep still owes is
// derived from the rows themselves and an interrupted one needs no progress record.
func (db *DB) ListStaleSecrets(ctx context.Context, keyID string, limit int) ([]StaleSecret, error) {
	var out []StaleSecret
	for i := range encryptedColumns {
		if len(out) >= limit {
			break
		}
		found, err := db.staleIn(ctx, &encryptedColumns[i], keyID, limit-len(out))
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// staleIn reads one column. limit 0 means every row.
func (db *DB) staleIn(ctx context.Context, c *encryptedColumn, keyID string, limit int) ([]StaleSecret, error) {
	if limit == 0 {
		limit = -1
	}
	// The key id is 1-64 characters of [A-Za-z0-9_-], so the prefix carries no LIKE
	// metacharacter and needs no escape clause.
	rows, err := db.Reader.QueryContext(ctx, c.stale, "v1."+keyID+".%", limit)
	if err != nil {
		return nil, fmt.Errorf("list stale %s.%s: %w", c.table, c.column, err)
	}
	defer func() { _ = rows.Close() }()

	var out []StaleSecret
	for rows.Next() {
		var id, envelope string
		if err := rows.Scan(&id, &envelope); err != nil {
			return nil, fmt.Errorf("scan stale %s.%s: %w", c.table, c.column, err)
		}
		out = append(out, StaleSecret{
			Table: c.table, Column: c.column, RowID: id,
			Purpose: c.purpose, Envelope: envelope,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale %s.%s: %w", c.table, c.column, err)
	}
	return out, nil
}

// ReplaceSecret rewrites one encrypted value, and only if it still holds the envelope the
// sweep read. It reports false when a concurrent writer got there first, which needs no
// retry: that writer sealed under the generation now active.
func (db *DB) ReplaceSecret(ctx context.Context, s *StaleSecret, sealed string) (bool, error) {
	for _, c := range encryptedColumns {
		if c.table != s.Table || c.column != s.Column {
			continue
		}
		res, err := db.Writer.ExecContext(ctx, c.replace, sealed, s.RowID, s.Envelope)
		if err != nil {
			return false, fmt.Errorf("re-encrypt %s.%s: %w", c.table, c.column, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("re-encrypt %s.%s: %w", c.table, c.column, err)
		}
		return n == 1, nil
	}
	return false, fmt.Errorf("%s.%s is not an encrypted column", s.Table, s.Column)
}
