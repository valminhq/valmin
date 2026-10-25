package store

import (
	"context"
	"fmt"
	"time"
)

// PlayerName is one name an account was seen with. Exact means the name was read on the same
// line as the platform id; Hits counts the character spawns attributed to the account by
// timing. On a host-wide listing both are summed over the servers.
type PlayerName struct {
	PlatformID  string
	Name        string
	Exact       bool
	Hits        int
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// RecordPlayerName upserts one name of an account on an instance. Hits add up, Exact once set
// stays set, and the seen window only widens.
func (db *DB) RecordPlayerName(ctx context.Context, instanceID string, n *PlayerName) error {
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO player_names (instance_id, platform_id, name, exact, hits, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance_id, platform_id, name) DO UPDATE SET
			exact = MAX(player_names.exact, excluded.exact),
			hits = player_names.hits + excluded.hits,
			first_seen_at = MIN(player_names.first_seen_at, excluded.first_seen_at),
			last_seen_at = MAX(player_names.last_seen_at, excluded.last_seen_at)`,
		instanceID, n.PlatformID, n.Name, n.Exact, n.Hits,
		FormatTime(n.FirstSeenAt), FormatTime(n.LastSeenAt))
	if err != nil {
		return fmt.Errorf("record name of %s on instance %s: %w", n.PlatformID, instanceID, err)
	}
	return nil
}

// ListPlayerNames returns every name recorded for one instance's accounts.
func (db *DB) ListPlayerNames(ctx context.Context, instanceID string) ([]PlayerName, error) {
	return db.queryPlayerNames(ctx, `
		SELECT platform_id, name, exact, hits, first_seen_at, last_seen_at
		FROM player_names WHERE instance_id = ?`, instanceID)
}

// ListAllPlayerNames returns every name recorded on the host, summed over the instances.
func (db *DB) ListAllPlayerNames(ctx context.Context) ([]PlayerName, error) {
	return db.queryPlayerNames(ctx, `
		SELECT platform_id, name, MAX(exact), SUM(hits), MIN(first_seen_at), MAX(last_seen_at)
		FROM player_names GROUP BY platform_id, name`)
}

func (db *DB) queryPlayerNames(ctx context.Context, query string, args ...any) ([]PlayerName, error) {
	rows, err := db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list player names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []PlayerName{}
	for rows.Next() {
		var n PlayerName
		var first, last string
		if err := rows.Scan(&n.PlatformID, &n.Name, &n.Exact, &n.Hits, &first, &last); err != nil {
			return nil, fmt.Errorf("scan player name: %w", err)
		}
		if n.FirstSeenAt, err = ParseTime(first); err != nil {
			return nil, fmt.Errorf("scan player name of %s: %w", n.PlatformID, err)
		}
		if n.LastSeenAt, err = ParseTime(last); err != nil {
			return nil, fmt.Errorf("scan player name of %s: %w", n.PlatformID, err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list player names: %w", err)
	}
	return out, nil
}

// HostPlayerIdentity is one account's row on one instance, with the instance's name.
type HostPlayerIdentity struct {
	PlayerIdentity
	InstanceID   string
	InstanceName string
}

// ListAllPlayerIdentities returns every instance's account rows, most recently seen first.
func (db *DB) ListAllPlayerIdentities(ctx context.Context) ([]HostPlayerIdentity, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT p.instance_id, i.name, p.platform_id, p.name, p.first_seen_at, p.last_seen_at
		FROM player_identities p JOIN instances i ON i.id = p.instance_id
		ORDER BY p.last_seen_at DESC, p.platform_id, i.name`)
	if err != nil {
		return nil, fmt.Errorf("list all player identities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []HostPlayerIdentity{}
	for rows.Next() {
		var h HostPlayerIdentity
		var first, last string
		if err := rows.Scan(&h.InstanceID, &h.InstanceName, &h.PlatformID, &h.Name, &first, &last); err != nil {
			return nil, fmt.Errorf("scan player identity: %w", err)
		}
		if h.FirstSeenAt, err = ParseTime(first); err != nil {
			return nil, fmt.Errorf("scan player identity %s: %w", h.PlatformID, err)
		}
		if h.LastSeenAt, err = ParseTime(last); err != nil {
			return nil, fmt.Errorf("scan player identity %s: %w", h.PlatformID, err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all player identities: %w", err)
	}
	return out, nil
}
