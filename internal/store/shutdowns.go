package store

import (
	"context"
	"fmt"
	"time"
)

// PlannedShutdown is a planned power cut: every running server is stopped shortly before
// PowerOffAt. CreatedBy is the panel account that planned it, nil for a Discord admin or a
// deleted account; CreatedByName names a planner who is not a panel account.
type PlannedShutdown struct {
	ID            string
	PowerOffAt    time.Time
	CreatedBy     *string
	CreatedByName string
	CreatedAt     time.Time
}

// CreatePlannedShutdown stores s together with audit, and removes the entries whose power cut
// has passed.
func (db *DB) CreatePlannedShutdown(ctx context.Context, s *PlannedShutdown, audit *AuditEntry) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin planned shutdown insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM planned_shutdowns WHERE power_off_at <= ?`, FormatTime(now)); err != nil {
		return fmt.Errorf("prune planned shutdowns: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO planned_shutdowns (id, power_off_at, created_by, created_by_name, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		s.ID, FormatTime(s.PowerOffAt), s.CreatedBy, s.CreatedByName, FormatTime(now)); err != nil {
		return fmt.Errorf("insert planned shutdown %s: %w", s.ID, err)
	}
	if audit != nil {
		if err := writeAuditLog(ctx, tx, audit, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit planned shutdown %s: %w", s.ID, err)
	}
	s.CreatedAt = now
	return nil
}

// DeletePlannedShutdown removes one entry together with audit. It reports false, and writes no
// audit, when no entry has that id.
func (db *DB) DeletePlannedShutdown(ctx context.Context, id string, audit *AuditEntry) (bool, error) {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin planned shutdown delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM planned_shutdowns WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete planned shutdown %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete planned shutdown %s: %w", id, err)
	}
	if n == 0 {
		return false, nil
	}
	if audit != nil {
		if err := writeAuditLog(ctx, tx, audit, time.Now().UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit planned shutdown delete %s: %w", id, err)
	}
	return true, nil
}

// PowerCutWithin reports whether a planned power cut falls after now and no later than
// now+window.
func (db *DB) PowerCutWithin(ctx context.Context, now time.Time, window time.Duration) (bool, error) {
	var soon bool
	if err := db.Reader.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM planned_shutdowns WHERE power_off_at > ? AND power_off_at <= ?)`,
		FormatTime(now), FormatTime(now.Add(window))).Scan(&soon); err != nil {
		return false, fmt.Errorf("read planned shutdowns: %w", err)
	}
	return soon, nil
}

// UpcomingShutdowns returns the entries whose power cut is after now, soonest first.
func (db *DB) UpcomingShutdowns(ctx context.Context, now time.Time) ([]PlannedShutdown, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT id, power_off_at, created_by, created_by_name, created_at
		FROM planned_shutdowns WHERE power_off_at > ? ORDER BY power_off_at, id`, FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list planned shutdowns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []PlannedShutdown{}
	for rows.Next() {
		var s PlannedShutdown
		var powerOff, created string
		if err := rows.Scan(&s.ID, &powerOff, &s.CreatedBy, &s.CreatedByName, &created); err != nil {
			return nil, fmt.Errorf("scan planned shutdown: %w", err)
		}
		if s.PowerOffAt, err = ParseTime(powerOff); err != nil {
			return nil, fmt.Errorf("planned shutdown %s power_off_at: %w", s.ID, err)
		}
		if s.CreatedAt, err = ParseTime(created); err != nil {
			return nil, fmt.Errorf("planned shutdown %s created_at: %w", s.ID, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list planned shutdowns: %w", err)
	}
	return out, nil
}
