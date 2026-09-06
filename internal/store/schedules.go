package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Schedule is one row of scheduled_jobs (04 §2): a cron expression, the kind it enqueues, and
// the clock's own bookkeeping. instance_id is nil for a global kind.
type Schedule struct {
	ID         string
	InstanceID *string
	Kind       string
	Cron       string
	Payload    string
	Enabled    bool
	LastRunAt  *time.Time
	NextRunAt  *time.Time
}

const scheduleColumns = `id, instance_id, kind, cron, payload, enabled, last_run_at, next_run_at`

func scanSchedule(s scanner) (Schedule, error) {
	var sc Schedule
	var instanceID, payload, lastRun, nextRun sql.NullString
	if err := s.Scan(
		&sc.ID, &instanceID, &sc.Kind, &sc.Cron, &payload, &sc.Enabled, &lastRun, &nextRun,
	); err != nil {
		return Schedule{}, fmt.Errorf("scan schedule row: %w", err)
	}
	if instanceID.Valid {
		sc.InstanceID = &instanceID.String
	}
	sc.Payload = payload.String
	for _, f := range []struct {
		ns  sql.NullString
		dst **time.Time
	}{{lastRun, &sc.LastRunAt}, {nextRun, &sc.NextRunAt}} {
		if !f.ns.Valid {
			continue
		}
		t, err := ParseTime(f.ns.String)
		if err != nil {
			return Schedule{}, fmt.Errorf("parse timestamp: %w", err)
		}
		*f.dst = &t
	}
	return sc, nil
}

// CreateSchedule inserts a schedule with its first next_run_at already computed, so a row is
// never due-by-being-unset.
func (db *DB) CreateSchedule(ctx context.Context, s *Schedule) error {
	if _, err := db.Writer.ExecContext(ctx, `
		INSERT INTO scheduled_jobs (id, instance_id, kind, cron, payload, enabled, next_run_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.InstanceID, s.Kind, s.Cron, s.Payload, s.Enabled, formatOrNil(s.NextRunAt),
	); err != nil {
		return fmt.Errorf("create schedule %s: %w", s.ID, err)
	}
	return nil
}

// UpdateSchedule writes the three fields an operator may change, plus the next_run_at the new
// expression implies. The kind and the instance are fixed at creation: changing either makes it
// a different schedule, and the job rows already pointing at this one would then describe
// something it never was.
func (db *DB) UpdateSchedule(ctx context.Context, s *Schedule) error {
	if _, err := db.Writer.ExecContext(ctx, `
		UPDATE scheduled_jobs SET cron = ?, payload = ?, enabled = ?, next_run_at = ? WHERE id = ?`,
		s.Cron, s.Payload, s.Enabled, formatOrNil(s.NextRunAt), s.ID,
	); err != nil {
		return fmt.Errorf("update schedule %s: %w", s.ID, err)
	}
	return nil
}

// DeleteSchedule removes a schedule. The job rows it produced keep their history: schedule_id
// is ON DELETE SET NULL, so deleting a schedule never deletes the record of what it ran.
func (db *DB) DeleteSchedule(ctx context.Context, id string) error {
	if _, err := db.Writer.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete schedule %s: %w", id, err)
	}
	return nil
}

// ScheduleByID reads one schedule. A missing row is (nil, nil).
func (db *DB) ScheduleByID(ctx context.Context, id string) (*Schedule, error) {
	row := db.Reader.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM scheduled_jobs WHERE id = ?`, scheduleColumns), id)
	s, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read schedule %s: %w", id, err)
	}
	return &s, nil
}

// ListSchedules returns every schedule, ordered so the listing is stable. There is no cursor:
// schedules are a handful of operator-written rows, not a growing log (11 §4 applies to
// collections that grow).
func (db *DB) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM scheduled_jobs ORDER BY instance_id, kind, id`, scheduleColumns))
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return collectSchedules(rows)
}

// DueSchedules returns the enabled schedules whose next_run_at has passed, plus any that have
// none yet — a row written before this build knew how to compute one still has to start
// somewhere.
func (db *DB) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM scheduled_jobs
		 WHERE enabled = TRUE AND (next_run_at IS NULL OR next_run_at <= ?)
		 ORDER BY next_run_at, id`, scheduleColumns), FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("read due schedules: %w", err)
	}
	return collectSchedules(rows)
}

func collectSchedules(rows *sql.Rows) ([]Schedule, error) {
	defer func() { _ = rows.Close() }()
	out := []Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schedules: %w", err)
	}
	return out, nil
}

// MarkScheduleRun advances a schedule past a tick, whether that tick enqueued a job or skipped
// one. last_run_at moves either way: it answers "when did the clock last consider this", which
// is what makes a run of skips visible rather than looking like a stopped clock.
func (db *DB) MarkScheduleRun(ctx context.Context, id string, lastRunAt, nextRunAt time.Time) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE scheduled_jobs SET last_run_at = ?, next_run_at = ? WHERE id = ?`,
		FormatTime(lastRunAt), FormatTime(nextRunAt), id,
	); err != nil {
		return fmt.Errorf("advance schedule %s: %w", id, err)
	}
	return nil
}

// RecordSkippedRun writes a terminal job row for a tick that enqueued nothing (ADR-030). It
// takes no lock and starts no work: the row exists so the skip is in the instance's job
// history, where an operator watching "last successful backup" will see it (12 §11).
func (db *DB) RecordSkippedRun(ctx context.Context, j *Job, code, message string) error {
	now := Now()
	if _, err := db.Writer.ExecContext(ctx, `
		INSERT INTO job_runs (
			id, kind, status, lock_key, instance_id, instance_name, schedule_id, payload,
			progress, error_code, error, created_at, started_at, finished_at
		) VALUES (?, ?, 'cancelled', ?, ?, ?, ?, '{}', 0, ?, ?, ?, ?, ?)`,
		j.ID, j.Kind, j.LockKey, j.InstanceID, j.InstanceName, j.ScheduleID,
		code, message, now, now, now,
	); err != nil {
		return fmt.Errorf("record skipped run of %s: %w", j.Kind, err)
	}
	return nil
}

func formatOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}
