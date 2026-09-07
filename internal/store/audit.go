package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AuditRecord is one row of the permanent trail as a reader sees it (04 §3). Actor and
// Instance are the joined names, nil once the user or instance is gone: the row outlives
// what it describes, so the ids stay and the names are best effort.
type AuditRecord struct {
	ID         string
	UserID     *string
	Actor      *string
	InstanceID *string
	Instance   *string
	Action     string
	Detail     *string
	IP         *string
	CreatedAt  time.Time
}

// AuditFilter is the closed allowlist of 04 §3. Empty fields do not filter.
type AuditFilter struct {
	InstanceID string
	UserID     string
	Action     string
}

// ListAuditLog returns audit rows newest first, one keyset page at a time (ADR-035):
// created_at with id breaking the tie, since a burst of rows shares a second.
func (db *DB) ListAuditLog(
	ctx context.Context, filter AuditFilter, beforeCreatedAt, beforeID string, limit int,
) ([]AuditRecord, error) {
	where := []string{"1 = 1"}
	var args []any
	for _, f := range []struct {
		column string
		value  string
	}{
		{"a.instance_id", filter.InstanceID},
		{"a.user_id", filter.UserID},
		{"a.action", filter.Action},
	} {
		if f.value != "" {
			where = append(where, f.column+" = ?")
			args = append(args, f.value)
		}
	}
	if beforeCreatedAt != "" {
		where = append(where, "(a.created_at < ? OR (a.created_at = ? AND a.id < ?))")
		args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	}
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`
		SELECT a.id, a.user_id, u.username, a.instance_id, i.name, a.action, a.detail, a.ip, a.created_at
		FROM audit_log a
		LEFT JOIN users u ON u.id = a.user_id
		LEFT JOIN instances i ON i.id = a.instance_id
		WHERE %s
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT ?`, strings.Join(where, " AND ")), args...)
	if err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []AuditRecord{}
	for rows.Next() {
		rec, err := scanAuditRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit log: %w", err)
	}
	return out, nil
}

func scanAuditRecord(rows *sql.Rows) (AuditRecord, error) {
	var rec AuditRecord
	var created string
	if err := rows.Scan(
		&rec.ID, &rec.UserID, &rec.Actor, &rec.InstanceID, &rec.Instance,
		&rec.Action, &rec.Detail, &rec.IP, &created,
	); err != nil {
		return AuditRecord{}, fmt.Errorf("scan audit row: %w", err)
	}
	at, err := ParseTime(created)
	if err != nil {
		return AuditRecord{}, fmt.Errorf("parse audit row time: %w", err)
	}
	rec.CreatedAt = at
	return rec, nil
}
