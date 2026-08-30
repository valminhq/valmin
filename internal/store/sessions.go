package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a row of 10 §4.1. The cookie value never lives here — only its hash does.
type Session struct {
	ID                string
	UserID            string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	IP                string
	UserAgent         string
}

// CreateSession inserts a new session row.
func (db *DB) CreateSession(ctx context.Context, s *Session, tokenHash string) error {
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO sessions (
			id, token_hash, user_id, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at, ip, user_agent
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, tokenHash, s.UserID, FormatTime(s.CreatedAt), FormatTime(s.LastSeenAt),
		FormatTime(s.IdleExpiresAt), FormatTime(s.AbsoluteExpiresAt), s.IP, s.UserAgent)
	if err != nil {
		return fmt.Errorf("create session for user %s: %w", s.UserID, err)
	}
	return nil
}

// SessionAndUser resolves a cookie's hash to the session and the user it belongs to.
//
// `↯` Expiry and revocation are filtered **in the SQL** — the same discipline D11 puts on
// grant expiry, for the same reason: a check a caller could forget is a check that will
// eventually be forgotten. `revoked_at` is carried by the schema but unused by this
// package: every M1 revocation path (logout, password change, role change, disable)
// deletes the row outright rather than marking it, so a live row is definitionally live.
// The filter stays as a second guard in case a future writer soft-deletes instead.
func (db *DB) SessionAndUser(ctx context.Context, tokenHash string) (*Session, *User, error) {
	var s Session
	var u User
	var lastLogin sql.NullString
	var createdAt, lastSeen, idleExp, absExp string

	err := db.Reader.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.created_at, s.last_seen_at,
		       s.idle_expires_at, s.absolute_expires_at, s.ip, s.user_agent,
		       u.id, u.username, u.role, u.disabled, u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?
		  AND s.revoked_at IS NULL
		  AND s.idle_expires_at > ?
		  AND s.absolute_expires_at > ?`,
		tokenHash, Now(), Now()).Scan(
		&s.ID, &s.UserID, &createdAt, &lastSeen, &idleExp, &absExp, &s.IP, &s.UserAgent,
		&u.ID, &u.Username, &u.Role, &u.Disabled, &u.CreatedAt, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("look up session: %w", err)
	}

	for _, pair := range []struct {
		dst *time.Time
		src string
	}{{&s.CreatedAt, createdAt}, {&s.LastSeenAt, lastSeen}, {&s.IdleExpiresAt, idleExp}, {&s.AbsoluteExpiresAt, absExp}} {
		if *pair.dst, err = ParseTime(pair.src); err != nil {
			return nil, nil, fmt.Errorf("look up session: %w", err)
		}
	}
	if lastLogin.Valid {
		t, err := ParseTime(lastLogin.String)
		if err != nil {
			return nil, nil, fmt.Errorf("look up session: %w", err)
		}
		u.LastLoginAt = &t
	}
	return &s, &u, nil
}

// TouchSession extends the idle expiry, but only if the last write was over throttle ago —
// 12 §... no, this is 10 §4.1's own rule: a live WebSocket must not hammer the single
// writer with a `last_seen_at` update on every frame. The WHERE clause makes the throttle
// atomic and stateless: a session touched within the window is simply not written.
func (db *DB) TouchSession(ctx context.Context, id string, now time.Time, idleTTL, throttle time.Duration) error {
	_, err := db.Writer.ExecContext(ctx, `
		UPDATE sessions SET last_seen_at = ?, idle_expires_at = ?
		WHERE id = ? AND last_seen_at <= ?`,
		FormatTime(now), FormatTime(now.Add(idleTTL)), id, FormatTime(now.Add(-throttle)))
	if err != nil {
		return fmt.Errorf("touch session %s: %w", id, err)
	}
	return nil
}

// DeleteSession removes one session row — an explicit logout (10 §4.1).
func (db *DB) DeleteSession(ctx context.Context, id string) error {
	if _, err := db.Writer.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// DeleteSessionsForUser removes every session belonging to userID — password change, role
// change and `disabled = 1` all reach live connections this way (10 §4.1). The hub half of
// "reach live connections" (dropping the socket, not just the future request) is WP-21's;
// deleting the row is what makes the *next* request from that session unauthenticated.
func (db *DB) DeleteSessionsForUser(ctx context.Context, userID string) error {
	if _, err := db.Writer.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete sessions for user %s: %w", userID, err)
	}
	return nil
}
