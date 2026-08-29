package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// leaseKey is the reserved kv key of 10 §4.2.
	leaseKey = "daemon_lease"
	// panelIDKey identifies the database itself, and survives restarts.
	panelIDKey = "panel_id"
	// LockFile sits next to the database and catches the common case — two panel
	// containers on one host — instantly and without a database round trip (12 §5.1).
	LockFile = ".valmind.lock"
)

// ErrLeaseHeld reports that another daemon holds a live lease on this database. It is a
// startup refusal rather than a wait: two daemons on one database is an operator error
// whose failure mode is two workers running one restore (ADR-031, C7).
var ErrLeaseHeld = errors.New("daemon lease held by another process")

// Lease is the value stored under kv["daemon_lease"] (12 §5.1).
type Lease struct {
	Owner     string    `json:"owner"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DaemonLease is one daemon's claim on one database: an flock on the local filesystem and
// a heartbeat row that reaches a second host.
type DaemonLease struct {
	db    *DB
	lock  *os.File
	owner string
	ttl   time.Duration
}

// AcquireDaemonLease takes both halves of the claim, or refuses. owner is
// "<panel_id>:<boot_id>" from Owner.
//
// Renew must be called for the lease to stay live; after a hard kill the stale lease
// blocks a restart for at most ttl, which is a self-healing delay rather than a lock
// needing a human.
func AcquireDaemonLease(ctx context.Context, db *DB, dataRoot, owner string, ttl time.Duration) (*DaemonLease, error) {
	// Opened through an os.Root so the lock cannot land outside data.root, which is the
	// same discipline every write under worlds/ follows (06 §4).
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		return nil, fmt.Errorf("open data.root %s: %w", dataRoot, err)
	}
	defer func() { _ = root.Close() }()

	path := filepath.Join(dataRoot, LockFile)
	f, err := root.OpenFile(LockFile, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another valmind already holds %s on this host: %w", path, ErrLeaseHeld)
	}

	l := &DaemonLease{db: db, lock: f, owner: owner, ttl: ttl}
	if err := l.claim(ctx); err != nil {
		_ = f.Close()
		return nil, err
	}
	slog.InfoContext(ctx, "daemon lease acquired",
		slog.String("owner", owner), slog.String("lock", path), slog.Duration("ttl", ttl))
	return l, nil
}

// Renew keeps the lease live until ctx is cancelled, refreshing every ttl/3.
//
// A renewal that finds another owner returns ErrLeaseHeld: continuing to run is the exact
// situation ADR-031 exists to prevent. A renewal that merely fails to reach the database
// is logged and retried, because taking the panel down over one SQLITE_BUSY is worse than
// the risk it avoids.
func (l *DaemonLease) Renew(ctx context.Context) error {
	tick := time.NewTicker(l.ttl / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := l.claim(ctx); err != nil {
				if errors.Is(err, ErrLeaseHeld) {
					return err
				}
				slog.WarnContext(ctx, "daemon lease renewal failed, will retry",
					slog.String("owner", l.owner), slog.Any("error", err))
			}
		}
	}
}

// Release drops both halves. Deleting the row is what lets the next start be immediate
// rather than waiting out the TTL.
func (l *DaemonLease) Release(ctx context.Context) error {
	err := l.withLeaseRow(ctx, func(tx *sql.Tx, cur *Lease) error {
		if cur != nil && cur.Owner != l.owner {
			return nil // someone else's lease; not ours to delete
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key = ?`, leaseKey); err != nil {
			return fmt.Errorf("delete lease row: %w", err)
		}
		return nil
	})
	return errors.Join(err, l.lock.Close())
}

// claim writes our lease unless another owner holds a live one.
func (l *DaemonLease) claim(ctx context.Context) error {
	return l.withLeaseRow(ctx, func(tx *sql.Tx, cur *Lease) error {
		now := time.Now().UTC()
		if cur != nil && cur.Owner != l.owner && cur.ExpiresAt.After(now) {
			return fmt.Errorf(
				"another valmind holds this database: host %s, pid %d, lease expires %s (in %s); "+
					"if that process is gone, wait for it to expire: %w",
				cur.Host, cur.PID, cur.ExpiresAt.Format(time.RFC3339),
				cur.ExpiresAt.Sub(now).Truncate(time.Second), ErrLeaseHeld)
		}

		host, _ := os.Hostname()
		raw, err := json.Marshal(Lease{
			Owner:     l.owner,
			Host:      host,
			PID:       os.Getpid(),
			ExpiresAt: now.Add(l.ttl),
		})
		if err != nil {
			return fmt.Errorf("encode lease: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			leaseKey, string(raw), now.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("write lease row: %w", err)
		}
		return nil
	})
}

// withLeaseRow runs fn against the current lease inside one writer transaction, so the
// read and the write cannot interleave with another daemon's.
func (l *DaemonLease) withLeaseRow(ctx context.Context, fn func(*sql.Tx, *Lease) error) error {
	tx, err := l.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("daemon lease: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var cur *Lease
	var raw string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, leaseKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("daemon lease: read: %w", err)
	default:
		var got Lease
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			return fmt.Errorf("daemon lease: decode: %w", err)
		}
		cur = &got
	}

	if err := fn(tx, cur); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("daemon lease: commit: %w", err)
	}
	return nil
}

// Owner returns this process's lease owner, "<panel_id>:<boot_id>" (12 §5.1). The panel id
// identifies the database and is created on first start; the boot id is fresh per process,
// which is what makes a job row left behind by a dead daemon recognisable (12 §9.1).
func Owner(ctx context.Context, db *DB) (string, error) {
	var panelID string
	found, err := db.KVGet(ctx, panelIDKey, &panelID)
	if err != nil {
		return "", err
	}
	if !found {
		panelID = randomID()
		if err := db.KVSet(ctx, panelIDKey, panelID); err != nil {
			return "", err
		}
	}
	return panelID + ":" + randomID(), nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error()) // unreachable on Linux; nothing sane to fall back to
	}
	return hex.EncodeToString(b)
}
