package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PlayerObservation is one recorded change in an instance's observed player count. A nil
// Players is an observation gap: the panel could no longer say, which is not the same fact as
// nobody being connected.
type PlayerObservation struct {
	ID         string
	InstanceID string
	ObservedAt time.Time
	Players    *int
}

// RecordPlayerObservation appends one row.
func (db *DB) RecordPlayerObservation(
	ctx context.Context, instanceID string, at time.Time, players *int,
) error {
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO player_observations (id, instance_id, observed_at, players)
		VALUES (?, ?, ?, ?)`, NewID(), instanceID, FormatTime(at), players)
	if err != nil {
		return fmt.Errorf("record player observation for instance %s: %w", instanceID, err)
	}
	return nil
}

// ListPlayerObservations returns instanceID's history newest first, keyset-paginated on
// (observed_at, id) so a page boundary stays stable when two rows share a timestamp.
func (db *DB) ListPlayerObservations(
	ctx context.Context, instanceID, beforeObservedAt, beforeID string, limit int,
) ([]PlayerObservation, error) {
	where, args := "instance_id = ?", []any{instanceID}
	if beforeObservedAt != "" {
		where += " AND (observed_at < ? OR (observed_at = ? AND id < ?))"
		args = append(args, beforeObservedAt, beforeObservedAt, beforeID)
	}
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, instance_id, observed_at, players
		FROM player_observations
		WHERE %s
		ORDER BY observed_at DESC, id DESC
		LIMIT ?`, where), args...)
	if err != nil {
		return nil, fmt.Errorf("list player observations of instance %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []PlayerObservation{}
	for rows.Next() {
		var (
			obs     PlayerObservation
			at      string
			players sql.NullInt64
		)
		if err := rows.Scan(&obs.ID, &obs.InstanceID, &at, &players); err != nil {
			return nil, fmt.Errorf("scan player observation: %w", err)
		}
		if obs.ObservedAt, err = ParseTime(at); err != nil {
			return nil, fmt.Errorf("scan player observation %s: %w", obs.ID, err)
		}
		if players.Valid {
			n := int(players.Int64)
			obs.Players = &n
		}
		out = append(out, obs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list player observations of instance %s: %w", instanceID, err)
	}
	return out, nil
}

// PrunePlayerObservations deletes observations older than before. It touches no other table:
// the audit trail of 12 §7 is permanent, and this history is not part of it.
func (db *DB) PrunePlayerObservations(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.Writer.ExecContext(ctx,
		`DELETE FROM player_observations WHERE observed_at < ?`, FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune player observations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune player observations: %w", err)
	}
	return n, nil
}

// PlayerIdentity is one account an instance's log named, with the window over which it was
// seen. Name is the display name last seen beside the id, and is empty for the lines that
// carry none.
type PlayerIdentity struct {
	PlatformID  string
	Name        string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// RecordPlayerIdentity upserts one account. A later sighting moves last_seen_at and keeps
// first_seen_at, and it replaces the display name only when it carries one: most sightings
// come from the socket line, which names no one, and letting those blank a name the history
// entry supplied would lose the only readable half of the row.
func (db *DB) RecordPlayerIdentity(
	ctx context.Context, instanceID string, id *PlayerIdentity,
) error {
	at := FormatTime(id.LastSeenAt)
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO player_identities (instance_id, platform_id, name, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (instance_id, platform_id) DO UPDATE SET
			name = CASE WHEN excluded.name = '' THEN player_identities.name ELSE excluded.name END,
			last_seen_at = MAX(player_identities.last_seen_at, excluded.last_seen_at)`,
		instanceID, id.PlatformID, id.Name, at, at)
	if err != nil {
		return fmt.Errorf("record player identity %s on instance %s: %w",
			id.PlatformID, instanceID, err)
	}
	return nil
}

// ListPlayerIdentities returns the accounts an instance has seen, most recent first. There is
// one row per account rather than one per sighting, so the list is short by construction and
// is not paginated.
func (db *DB) ListPlayerIdentities(ctx context.Context, instanceID string) ([]PlayerIdentity, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT platform_id, name, first_seen_at, last_seen_at
		FROM player_identities
		WHERE instance_id = ?
		ORDER BY last_seen_at DESC, platform_id`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list player identities of instance %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []PlayerIdentity{}
	for rows.Next() {
		var id PlayerIdentity
		var first, last string
		if err := rows.Scan(&id.PlatformID, &id.Name, &first, &last); err != nil {
			return nil, fmt.Errorf("scan player identity: %w", err)
		}
		if id.FirstSeenAt, err = ParseTime(first); err != nil {
			return nil, fmt.Errorf("scan player identity %s: %w", id.PlatformID, err)
		}
		if id.LastSeenAt, err = ParseTime(last); err != nil {
			return nil, fmt.Errorf("scan player identity %s: %w", id.PlatformID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list player identities of instance %s: %w", instanceID, err)
	}
	return out, nil
}
