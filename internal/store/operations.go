package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Operation states. An operation is open while it is running or interrupted, and one
// instance has at most one open operation.
const (
	OperationRunning     = "running"
	OperationInterrupted = "interrupted"
	OperationCompleted   = "completed"
	OperationAbandoned   = "abandoned"
)

// Operation is one instance-definition chain's persisted intent (Q52). Steps and Plan hold
// opaque JSON owned by the api layer; the store only keeps the cursor and state consistent.
type Operation struct {
	ID         string
	InstanceID string
	Kind       string
	State      string
	Steps      string
	Cursor     int
	Plan       string
	CreatedBy  *string
	CreatedAt  string
	UpdatedAt  string
}

const operationColumns = `id, instance_id, kind, state, steps, cursor, plan, created_by, created_at, updated_at`

// CreateOperation inserts a new running operation. It fails if the instance already has an
// open one.
func (db *DB) CreateOperation(ctx context.Context, op *Operation) error {
	now := Now()
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO instance_operations (`+operationColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.InstanceID, op.Kind, op.State, op.Steps, op.Cursor, op.Plan, op.CreatedBy, now, now)
	if err != nil {
		return fmt.Errorf("create operation for instance %s: %w", op.InstanceID, err)
	}
	op.CreatedAt, op.UpdatedAt = now, now
	return nil
}

// OpenOperation reads the instance's outstanding operation, or nil when there is none.
func (db *DB) OpenOperation(ctx context.Context, instanceID string) (*Operation, error) {
	return scanOperation(db.Reader.QueryRowContext(ctx, `
		SELECT `+operationColumns+` FROM instance_operations
		WHERE instance_id = ? AND state IN ('running', 'interrupted')`, instanceID))
}

// TxOpenOperation is OpenOperation inside a caller's transaction.
func TxOpenOperation(ctx context.Context, tx *sql.Tx, instanceID string) (*Operation, error) {
	return scanOperation(tx.QueryRowContext(ctx, `
		SELECT `+operationColumns+` FROM instance_operations
		WHERE instance_id = ? AND state IN ('running', 'interrupted')`, instanceID))
}

func scanOperation(row *sql.Row) (*Operation, error) {
	var op Operation
	err := row.Scan(&op.ID, &op.InstanceID, &op.Kind, &op.State, &op.Steps, &op.Cursor,
		&op.Plan, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read operation: %w", err)
	}
	return &op, nil
}

// TxAdvanceOperation records a completed step and moves the cursor, inside the finish
// transaction of the job that completed it. cursor is the index of the next outstanding
// step, and state is 'completed' once no step remains.
func TxAdvanceOperation(ctx context.Context, tx *sql.Tx, id, steps string, cursor int, state string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE instance_operations SET steps = ?, cursor = ?, state = ?, updated_at = ?
		WHERE id = ?`, steps, cursor, state, Now(), id)
	if err != nil {
		return fmt.Errorf("advance operation %s: %w", id, err)
	}
	return nil
}

// SetOperationState moves an open operation to a new state.
func (db *DB) SetOperationState(ctx context.Context, id, state string) error {
	_, err := db.Writer.ExecContext(ctx, `
		UPDATE instance_operations SET state = ?, updated_at = ?
		WHERE id = ? AND state IN ('running', 'interrupted')`, state, Now(), id)
	if err != nil {
		return fmt.Errorf("set operation %s state: %w", id, err)
	}
	return nil
}

// InterruptIdleOperations marks every running operation whose instance holds no live job as
// interrupted, and reports how many it moved. It is the startup pass that makes a chain cut
// by a crash visible instead of silently stalled; nothing is replayed by it.
func (db *DB) InterruptIdleOperations(ctx context.Context) (int, error) {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE instance_operations SET state = 'interrupted', updated_at = ?
		WHERE state = 'running' AND instance_id NOT IN (
			SELECT instance_id FROM job_runs
			WHERE instance_id IS NOT NULL AND status IN ('queued', 'running')
		)`, Now())
	if err != nil {
		return 0, fmt.Errorf("interrupt idle operations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("interrupt idle operations: %w", err)
	}
	return int(n), nil
}
