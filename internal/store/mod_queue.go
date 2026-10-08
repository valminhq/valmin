package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// QueuedModInstall is a mod install requested while its server ran, waiting for it to stop.
// Source and RequestedBy are empty when the request named none.
type QueuedModInstall struct {
	InstanceID  string
	FullName    string
	Version     string
	Source      string
	RequestedBy string
	CreatedAt   time.Time
}

const queueModInstallSQL = `
	INSERT INTO queued_mod_installs (instance_id, full_name, version, source, requested_by, created_at)
	VALUES (?, ?, ?, ?, NULLIF(?, ''), ?)
	ON CONFLICT (instance_id, full_name) DO UPDATE SET
		version = excluded.version, source = excluded.source, requested_by = excluded.requested_by`

// QueueModInstall records q, replacing a queued install of the same package. The replacement
// keeps the package's place in the queue.
func (db *DB) QueueModInstall(ctx context.Context, q *QueuedModInstall) error {
	return queueModInstall(ctx, db.Writer, q)
}

// QueueModInstalls records every entry of qs as QueueModInstall does, all or none.
func (db *DB) QueueModInstalls(ctx context.Context, qs []QueuedModInstall) error {
	return db.inTx(ctx, "queue mod installs", func(tx *sql.Tx) error {
		for i := range qs {
			if err := queueModInstall(ctx, tx, &qs[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func queueModInstall(ctx context.Context, ex execer, q *QueuedModInstall) error {
	if _, err := ex.ExecContext(ctx, queueModInstallSQL,
		q.InstanceID, q.FullName, q.Version, q.Source, q.RequestedBy, Now(),
	); err != nil {
		return fmt.Errorf("queue install of %s on instance %s: %w", q.FullName, q.InstanceID, err)
	}
	return nil
}

// QueuedModInstalls lists an instance's queued installs, oldest first.
func (db *DB) QueuedModInstalls(ctx context.Context, instanceID string) ([]QueuedModInstall, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT instance_id, full_name, version, source, COALESCE(requested_by, ''), created_at
		FROM queued_mod_installs WHERE instance_id = ? ORDER BY created_at, full_name`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list queued installs of instance %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []QueuedModInstall
	for rows.Next() {
		var q QueuedModInstall
		var created string
		if err := rows.Scan(&q.InstanceID, &q.FullName, &q.Version, &q.Source, &q.RequestedBy, &created); err != nil {
			return nil, fmt.Errorf("scan queued install: %w", err)
		}
		if q.CreatedAt, err = ParseTime(created); err != nil {
			return nil, fmt.Errorf("queued install of %s: %w", q.FullName, err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued installs of instance %s: %w", instanceID, err)
	}
	return out, nil
}

// UnqueueModInstall removes a queued install and reports whether there was one.
func (db *DB) UnqueueModInstall(ctx context.Context, instanceID, fullName string) (bool, error) {
	res, err := db.Writer.ExecContext(ctx,
		`DELETE FROM queued_mod_installs WHERE instance_id = ? AND full_name = ?`, instanceID, fullName)
	if err != nil {
		return false, fmt.Errorf("unqueue install of %s on instance %s: %w", fullName, instanceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unqueue install of %s on instance %s: %w", fullName, instanceID, err)
	}
	return n > 0, nil
}

// UnqueueModInstalls removes the queued installs of every named package.
func (db *DB) UnqueueModInstalls(ctx context.Context, instanceID string, fullNames []string) error {
	return db.inTx(ctx, "unqueue mod installs", func(tx *sql.Tx) error {
		for _, name := range fullNames {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM queued_mod_installs WHERE instance_id = ? AND full_name = ?`, instanceID, name,
			); err != nil {
				return fmt.Errorf("unqueue install of %s on instance %s: %w", name, instanceID, err)
			}
		}
		return nil
	})
}

// InstancesWithModQueue lists the instances holding a queued install or an owed start.
func (db *DB) InstancesWithModQueue(ctx context.Context) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT instance_id FROM queued_mod_installs
		UNION SELECT instance_id FROM queued_mod_starts`)
	if err != nil {
		return nil, fmt.Errorf("list instances with a mod queue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan instance with a mod queue: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list instances with a mod queue: %w", err)
	}
	return out, nil
}

// TxQueueModStart records, in the transaction that stops a restarting server, that it is owed
// a start once its queued installs have run.
func TxQueueModStart(ctx context.Context, tx *sql.Tx, instanceID, requestedBy string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO queued_mod_starts (instance_id, requested_by, created_at) VALUES (?, NULLIF(?, ''), ?)
		ON CONFLICT (instance_id) DO NOTHING`, instanceID, requestedBy, Now()); err != nil {
		return fmt.Errorf("queue start of instance %s: %w", instanceID, err)
	}
	return nil
}

// QueuedModStart reports whether an instance is owed a start, and who asked for it.
func (db *DB) QueuedModStart(ctx context.Context, instanceID string) (owed bool, requestedBy string, err error) {
	err = db.Reader.QueryRowContext(ctx,
		`SELECT COALESCE(requested_by, '') FROM queued_mod_starts WHERE instance_id = ?`, instanceID,
	).Scan(&requestedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read queued start of instance %s: %w", instanceID, err)
	}
	return true, requestedBy, nil
}

// ClearQueuedModStart drops an instance's owed start.
func (db *DB) ClearQueuedModStart(ctx context.Context, instanceID string) error {
	if _, err := db.Writer.ExecContext(ctx,
		`DELETE FROM queued_mod_starts WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("clear queued start of instance %s: %w", instanceID, err)
	}
	return nil
}
