package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Job is a row of 12 §4.1 — the operational record for one long-running operation.
// job_locks is not modelled here: it is write-only from this package's point of view,
// visible only through the conflict ClaimJob reports.
type Job struct {
	ID                string
	Kind              string
	Status            string // queued|running|succeeded|failed|cancelled (12 §4.1)
	LockKey           string
	InstanceID        *string
	InstanceName      string
	Payload           string // JSON, kind-specific (12 §4.1)
	Checkpoint        *string
	ResumeAfter       bool
	Progress          int
	Message           *string
	LeaseOwner        *string
	LeaseUntil        *time.Time
	CancelRequestedAt *time.Time
	RequestedBy       *string
	Attempt           int
	ErrorCode         *string
	Error             *string
	Log               *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

// JobConflict reports that lock_key is already held. ADR-030: reject, don't queue — the
// caller subscribes to the active job instead of retrying (12 §4.3, 11 §2.5's
// job_in_progress).
type JobConflict struct {
	JobID string
	Kind  string
}

func (e *JobConflict) Error() string {
	return fmt.Sprintf("job %s (kind %s) already holds this lock", e.JobID, e.Kind)
}

// ClaimJob is 12 §6's Claim phase, entire: acquire the lock, insert the job row already
// `running` with its lease, and — inside the same transaction — let onClaim make whatever
// side-effect change the caller's kind requires (an instance's transient state, most
// often). `↯` Nothing here calls Docker, the filesystem or the network (C1) — onClaim gets
// a *sql.Tx, not a context to do work with.
//
// A lock_key collision is not an error to log and retry: it is ADR-030's answer, returned
// as *JobConflict so the caller can hand the client the active job's id.
func (db *DB) ClaimJob(
	ctx context.Context, j *Job, owner string, leaseUntil time.Time, onClaim func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("claim job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := Now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		j.LockKey, j.ID, now,
	); err != nil {
		if isUniqueViolation(err) {
			var conflict JobConflict
			row := tx.QueryRowContext(ctx, `
				SELECT jr.id, jr.kind FROM job_locks jl JOIN job_runs jr ON jr.id = jl.job_id
				WHERE jl.lock_key = ?`, j.LockKey)
			if scanErr := row.Scan(&conflict.JobID, &conflict.Kind); scanErr != nil {
				return fmt.Errorf("look up holder of lock %s: %w", j.LockKey, scanErr)
			}
			return &conflict
		}
		return fmt.Errorf("acquire lock %s: %w", j.LockKey, err)
	}

	nowT, err := ParseTime(now)
	if err != nil {
		return fmt.Errorf("claim job: parse timestamp: %w", err)
	}
	j.Status = "running"
	j.LeaseOwner = &owner
	j.LeaseUntil = &leaseUntil
	j.CreatedAt = nowT
	j.StartedAt = &nowT
	j.Attempt = 1
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO job_runs (
			id, kind, status, lock_key, instance_id, instance_name, payload,
			resume_after, progress, lease_owner, lease_until, requested_by, attempt, created_at, started_at
		) VALUES (?, ?, 'running', ?, ?, ?, ?, ?, 0, ?, ?, ?, 1, ?, ?)`,
		j.ID, j.Kind, j.LockKey, j.InstanceID, j.InstanceName, j.Payload,
		j.ResumeAfter, owner, FormatTime(leaseUntil), j.RequestedBy, now, now,
	); err != nil {
		return fmt.Errorf("insert job run %s: %w", j.ID, err)
	}

	if onClaim != nil {
		if err := onClaim(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("claim job: commit: %w", err)
	}
	return nil
}

// RenewJobLease is the single autocommit UPDATE of 12 §5.2, run outside any transaction.
// It reports false when no row matched — the row's lease_owner is no longer ours, which
// C17 makes fatal to the job, never to the panel.
func (db *DB) RenewJobLease(ctx context.Context, jobID, owner string, until time.Time) (bool, error) {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE job_runs SET lease_until = ?
		WHERE id = ? AND lease_owner = ? AND status = 'running'`,
		FormatTime(until), jobID, owner)
	if err != nil {
		return false, fmt.Errorf("renew lease for job %s: %w", jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew lease for job %s: %w", jobID, err)
	}
	return n == 1, nil
}

// UpdateJobProgress is the throttled, single-statement write of 12 §7. Throttling by time
// and by change is the caller's job (jobs.Handle) — this always writes.
func (db *DB) UpdateJobProgress(ctx context.Context, jobID string, progress int, message string) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE job_runs SET progress = ?, message = ? WHERE id = ?`, progress, message, jobID,
	); err != nil {
		return fmt.Errorf("update progress for job %s: %w", jobID, err)
	}
	return nil
}

// FinishJob is 12 §6's Finish phase: terminal status, lock release and onFinish's
// side-effect rows, in one transaction, from data already in memory (12 §6's corollary —
// this never reads to decide what to write).
func (db *DB) FinishJob(
	ctx context.Context, jobID, status string, progress int, errorCode, errMsg, log *string,
	finishedAt time.Time, onFinish func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE job_runs SET status = ?, progress = ?, error_code = ?, error = ?, log = ?,
			lease_owner = NULL, lease_until = NULL, finished_at = ?
		WHERE id = ?`,
		status, progress, errorCode, errMsg, log, FormatTime(finishedAt), jobID,
	); err != nil {
		return fmt.Errorf("finish job %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_locks WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("release lock for job %s: %w", jobID, err)
	}
	if onFinish != nil {
		if err := onFinish(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish job: commit: %w", err)
	}
	return nil
}

// CancelQueuedJob is 12 §8's queued row: cancel and release the lock immediately, in one
// transaction, with no worker to notify. It reports false if the row was not (or is no
// longer) queued.
func (db *DB) CancelQueuedJob(ctx context.Context, jobID string, now time.Time) (bool, error) {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("cancel queued job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE job_runs SET status = 'cancelled', finished_at = ? WHERE id = ? AND status = 'queued'`,
		FormatTime(now), jobID)
	if err != nil {
		return false, fmt.Errorf("cancel queued job %s: %w", jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel queued job %s: %w", jobID, err)
	}
	if n != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_locks WHERE job_id = ?`, jobID); err != nil {
		return false, fmt.Errorf("release lock for job %s: %w", jobID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("cancel queued job: commit: %w", err)
	}
	return true, nil
}

// RequestJobCancel sets cancel_requested_at (12 §8) — a request, not an effect. The
// worker notices it at its next boundary; the row's checkpoint decides whether that
// boundary exists at all.
func (db *DB) RequestJobCancel(ctx context.Context, jobID string, now time.Time) error {
	if _, err := db.Writer.ExecContext(ctx, `
		UPDATE job_runs SET cancel_requested_at = ? WHERE id = ? AND cancel_requested_at IS NULL`,
		FormatTime(now), jobID,
	); err != nil {
		return fmt.Errorf("request cancel for job %s: %w", jobID, err)
	}
	return nil
}

const jobColumns = `id, kind, status, lock_key, instance_id, instance_name, payload,
	checkpoint, resume_after, progress, message, lease_owner, lease_until, cancel_requested_at,
	requested_by, attempt, error_code, error, log, created_at, started_at, finished_at`

func scanJob(s scanner) (Job, error) {
	var j Job
	var instanceID, checkpoint, message, leaseOwner, leaseUntil, cancelAt sql.NullString
	var requestedBy, errorCode, errMsg, logVal, startedAt, finishedAt sql.NullString
	var createdAt string

	if err := s.Scan(
		&j.ID, &j.Kind, &j.Status, &j.LockKey, &instanceID, &j.InstanceName, &j.Payload,
		&checkpoint, &j.ResumeAfter, &j.Progress, &message, &leaseOwner, &leaseUntil, &cancelAt,
		&requestedBy, &j.Attempt, &errorCode, &errMsg, &logVal, &createdAt, &startedAt, &finishedAt,
	); err != nil {
		return Job{}, fmt.Errorf("scan job row: %w", err)
	}

	var err error
	if j.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Job{}, fmt.Errorf("created_at: %w", err)
	}
	for _, f := range []struct {
		ns  sql.NullString
		dst **string
	}{
		{instanceID, &j.InstanceID},
		{checkpoint, &j.Checkpoint},
		{message, &j.Message},
		{leaseOwner, &j.LeaseOwner},
		{requestedBy, &j.RequestedBy},
		{errorCode, &j.ErrorCode},
		{errMsg, &j.Error},
		{logVal, &j.Log},
	} {
		if f.ns.Valid {
			v := f.ns.String
			*f.dst = &v
		}
	}
	for _, f := range []struct {
		ns  sql.NullString
		dst **time.Time
	}{{leaseUntil, &j.LeaseUntil}, {cancelAt, &j.CancelRequestedAt}, {startedAt, &j.StartedAt}, {finishedAt, &j.FinishedAt}} {
		if f.ns.Valid {
			t, err := ParseTime(f.ns.String)
			if err != nil {
				return Job{}, fmt.Errorf("parse timestamp: %w", err)
			}
			*f.dst = &t
		}
	}
	return j, nil
}

// JobByID reads one job row, for GET /jobs/{id} and for cancellation. A missing job is
// (nil, nil), the same "not found is not a failure" shape as every other lookup.
func (db *DB) JobByID(ctx context.Context, id string) (*Job, error) {
	row := db.Reader.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM job_runs WHERE id = ?`, jobColumns), id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up job %s: %w", id, err)
	}
	return &j, nil
}

// SweepTerminalJobs is 12 §7's retention sweep: one DELETE, run once at daemon start.
// A row is pruned once it is older than retentionDays, or once it falls outside the most
// recent 500 terminal rows for its instance_id (global jobs — instance_id IS NULL — share
// one such group) — whichever bites first.
func (db *DB) SweepTerminalJobs(ctx context.Context, now time.Time, retentionDays int) (int64, error) {
	cutoff := FormatTime(now.AddDate(0, 0, -retentionDays))
	res, err := db.Writer.ExecContext(ctx, `
		DELETE FROM job_runs WHERE id IN (
			SELECT id FROM (
				SELECT id, created_at,
				       ROW_NUMBER() OVER (PARTITION BY instance_id ORDER BY created_at DESC) AS rn
				FROM job_runs
				WHERE status IN ('succeeded', 'failed', 'cancelled')
			) ranked
			WHERE rn > 500 OR created_at < ?
		)`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sweep terminal jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep terminal jobs: %w", err)
	}
	return n, nil
}
