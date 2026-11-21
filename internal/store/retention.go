package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SweepExpired deletes panel rows past their use, in one transaction: sessions that expired or
// were revoked, conditions resolved before now-keep (their notifications cascade), and terminal
// deliveries last updated before now-keep. A delivery of a condition still kept survives, since
// resolving that condition edits the message its opening delivery posted.
func (db *DB) SweepExpired(ctx context.Context, now time.Time, keep time.Duration) error {
	stamp, cutoff := FormatTime(now), FormatTime(now.Add(-keep))
	stmts := []struct {
		what, query string
		args        []any
	}{
		{
			"sessions", `DELETE FROM sessions
			WHERE idle_expires_at <= ? OR absolute_expires_at <= ? OR revoked_at IS NOT NULL`,
			[]any{stamp, stamp},
		},
		// Conditions go first: their notifications cascade, so the ones left name the events
		// of kept conditions only.
		{"resolved conditions", `DELETE FROM alert_conditions WHERE resolved_at < ?`, []any{cutoff}},
		{
			"deliveries", `DELETE FROM webhook_deliveries
			WHERE status IN ('delivered', 'failed') AND updated_at < ?
			AND event_id NOT IN (SELECT event_id FROM alert_notifications WHERE event_id IS NOT NULL)`,
			[]any{cutoff},
		},
	}
	return db.inTx(ctx, "sweep expired rows", func(tx *sql.Tx) error {
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s.query, s.args...); err != nil {
				return fmt.Errorf("sweep expired %s: %w", s.what, err)
			}
		}
		return nil
	})
}
