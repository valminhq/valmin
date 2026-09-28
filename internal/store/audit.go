package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// AuditRecord is one row of the permanent trail as a reader sees it. Actor and Instance are the
// names recorded when the entry was written, falling back to the current name for older rows;
// the ids outlive both.
type AuditRecord struct {
	ID         string
	UserID     *string
	Actor      *string
	InstanceID *string
	Instance   *string
	Action     string
	Detail     *string
	IP         *string
	Outcome    *string
	JobID      *string
	JobStatus  *string
	JobError   *string
	CreatedAt  time.Time
}

// AuditFilter is the closed allowlist of audit filters. Empty fields do not filter; Since is
// inclusive and Until exclusive.
type AuditFilter struct {
	InstanceID string
	UserID     string
	Action     string
	Since      time.Time
	Until      time.Time
}

const auditSelect = `
	SELECT a.id, a.user_id, COALESCE(a.actor_name, u.username), a.instance_id,
	       COALESCE(a.instance_name, i.name), a.action, a.detail, a.ip, a.outcome,
	       a.job_id, j.status, j.error, a.created_at
	FROM audit_log a
	LEFT JOIN users u ON u.id = a.user_id
	LEFT JOIN instances i ON i.id = a.instance_id
	LEFT JOIN job_runs j ON j.id = a.job_id`

// ListAuditLog returns audit rows newest first, one keyset page at a time: created_at with id
// breaking the tie, since a burst of rows shares a second.
func (db *DB) ListAuditLog(
	ctx context.Context, filter *AuditFilter, beforeCreatedAt, beforeID string, limit int,
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
	if !filter.Since.IsZero() {
		where = append(where, "a.created_at >= ?")
		args = append(args, FormatTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		where = append(where, "a.created_at < ?")
		args = append(args, FormatTime(filter.Until))
	}
	if beforeCreatedAt != "" {
		where = append(where, "(a.created_at < ? OR (a.created_at = ? AND a.id < ?))")
		args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	}
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`%s
		WHERE %s
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT ?`, auditSelect, strings.Join(where, " AND ")), args...)
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
		&rec.Action, &rec.Detail, &rec.IP, &rec.Outcome,
		&rec.JobID, &rec.JobStatus, &rec.JobError, &created,
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

// AuditSubject is a user or instance that appears in the trail, named as last recorded.
type AuditSubject struct {
	ID   string
	Name string
}

// AuditFacets is the vocabulary the trail's filters offer: what the log actually contains,
// including users and servers that no longer exist.
type AuditFacets struct {
	Actions   []string
	Actors    []AuditSubject
	Instances []AuditSubject
}

// AuditFacets lists every distinct action, actor and instance in the trail. A subject's name is
// its most recent recorded one, or its current name when no entry recorded any.
func (db *DB) AuditFacets(ctx context.Context) (AuditFacets, error) {
	var facets AuditFacets
	actions, err := db.Reader.QueryContext(ctx, `SELECT DISTINCT action FROM audit_log ORDER BY action`)
	if err != nil {
		return AuditFacets{}, fmt.Errorf("list audit actions: %w", err)
	}
	defer func() { _ = actions.Close() }()
	for actions.Next() {
		var action string
		if err := actions.Scan(&action); err != nil {
			return AuditFacets{}, fmt.Errorf("scan audit action: %w", err)
		}
		facets.Actions = append(facets.Actions, action)
	}
	if err := actions.Err(); err != nil {
		return AuditFacets{}, fmt.Errorf("list audit actions: %w", err)
	}

	if facets.Actors, err = db.auditSubjects(ctx, "user_id", "actor_name", "users", "username"); err != nil {
		return AuditFacets{}, err
	}
	if facets.Instances, err = db.auditSubjects(ctx, "instance_id", "instance_name", "instances", "name"); err != nil {
		return AuditFacets{}, err
	}
	return facets, nil
}

// auditSubjects reads the distinct ids in one audit column with the newest name recorded beside
// them, falling back to the live table. The arguments are compile-time constants.
func (db *DB) auditSubjects(
	ctx context.Context,
	idColumn, nameColumn, table, tableName string,
) ([]AuditSubject, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id, COALESCE(
			(SELECT a.%[2]s FROM audit_log a
			 WHERE a.%[1]s = s.id AND a.%[2]s IS NOT NULL
			 ORDER BY a.created_at DESC, a.id DESC LIMIT 1),
			(SELECT t.%[4]s FROM %[3]s t WHERE t.id = s.id), '')
		FROM (SELECT DISTINCT %[1]s AS id FROM audit_log WHERE %[1]s IS NOT NULL) s
		ORDER BY 2, 1`, idColumn, nameColumn, table, tableName))
	if err != nil {
		return nil, fmt.Errorf("list audit %s: %w", idColumn, err)
	}
	defer func() { _ = rows.Close() }()

	var out []AuditSubject
	for rows.Next() {
		var s AuditSubject
		if err := rows.Scan(&s.ID, &s.Name); err != nil {
			return nil, fmt.Errorf("scan audit %s: %w", idColumn, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit %s: %w", idColumn, err)
	}
	return out, nil
}
