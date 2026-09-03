package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Invite is a row of 09 §5: single-use, expiring, optionally pre-bound to an instance and
// grant. The plaintext code is never stored — only its hash, and only this package ever
// reads token_hash back out.
type Invite struct {
	ID         string
	CreatedBy  string
	InstanceID *string
	GrantRole  *GrantRole
	GrantPerms []string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	RedeemedAt *time.Time
	RedeemedBy *string
	RevokedAt  *time.Time
}

// Live reports whether the invite can still be redeemed. Every caller uses this rather
// than inspecting the fields directly, because 09 §5 wants exactly one answer —
// invite_invalid — for expired, revoked, redeemed and never-existed alike, and a second
// spelling of "is this still good" is a second place to get the boundary wrong.
func (inv *Invite) Live(now time.Time) bool {
	return inv.RevokedAt == nil && inv.RedeemedAt == nil && inv.ExpiresAt.After(now)
}

// CreateInvite inserts a new invite. permsJSON is the caller's already-encoded perms
// array, so this package does not need to know the shape authz.Action gives it.
func (db *DB) CreateInvite(ctx context.Context, inv *Invite, tokenHash, permsJSON string) error {
	var grantRole any
	if inv.GrantRole != nil {
		grantRole = string(*inv.GrantRole)
	}
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO invites (
			id, token_hash, created_by, instance_id, grant_role, grant_perms,
			expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, tokenHash, inv.CreatedBy, inv.InstanceID, grantRole, permsJSON,
		FormatTime(inv.ExpiresAt), FormatTime(inv.CreatedAt))
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	return nil
}

const inviteColumns = `id, created_by, instance_id, grant_role, grant_perms,
	expires_at, created_at, redeemed_at, redeemed_by, revoked_at`

// scanInvite reads one inviteColumns row. When tokenHash is non-nil it must be the first
// column selected — sql.Rows.Scan takes every destination in one call, so a hash scanned
// alongside the rest has to be part of the same dest slice, not a second Scan on the same
// row.
func scanInvite(s scanner, tokenHash *string) (Invite, error) {
	var inv Invite
	var instanceID, grantRole, permsJSON, redeemedAt, redeemedBy, revokedAt sql.NullString
	var expiresAt, createdAt string

	dest := []any{
		&inv.ID, &inv.CreatedBy, &instanceID, &grantRole, &permsJSON,
		&expiresAt, &createdAt, &redeemedAt, &redeemedBy, &revokedAt,
	}
	if tokenHash != nil {
		dest = append([]any{tokenHash}, dest...)
	}
	if err := s.Scan(dest...); err != nil {
		return Invite{}, fmt.Errorf("scan invite row: %w", err)
	}

	var err error
	if inv.ExpiresAt, err = ParseTime(expiresAt); err != nil {
		return Invite{}, fmt.Errorf("expires_at: %w", err)
	}
	if inv.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Invite{}, fmt.Errorf("created_at: %w", err)
	}
	if instanceID.Valid {
		inv.InstanceID = &instanceID.String
	}
	if grantRole.Valid {
		r := GrantRole(grantRole.String)
		inv.GrantRole = &r
	}
	if permsJSON.Valid && permsJSON.String != "" {
		if err := json.Unmarshal([]byte(permsJSON.String), &inv.GrantPerms); err != nil {
			return Invite{}, fmt.Errorf("grant_perms: %w", err)
		}
	}
	if redeemedAt.Valid {
		t, err := ParseTime(redeemedAt.String)
		if err != nil {
			return Invite{}, fmt.Errorf("redeemed_at: %w", err)
		}
		inv.RedeemedAt = &t
	}
	if redeemedBy.Valid {
		inv.RedeemedBy = &redeemedBy.String
	}
	if revokedAt.Valid {
		t, err := ParseTime(revokedAt.String)
		if err != nil {
			return Invite{}, fmt.Errorf("revoked_at: %w", err)
		}
		inv.RevokedAt = &t
	}
	return inv, nil
}

// InviteByID is what the issuing admin's own view (list, revoke) resolves by.
func (db *DB) InviteByID(ctx context.Context, id string) (*Invite, error) {
	return db.lookupInvite(ctx, `id = ?`, id)
}

func (db *DB) lookupInvite(ctx context.Context, where, arg string) (*Invite, error) {
	row := db.Reader.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM invites WHERE %s`, inviteColumns, where), arg)
	inv, err := scanInvite(row, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // "no such invite" is the common answer, not a failure
	}
	if err != nil {
		return nil, fmt.Errorf("look up invite: %w", err)
	}
	return &inv, nil
}

// InviteRecord carries the argon2id hash a live scan verifies against — the one place
// token_hash leaves the store, mirroring AuthRecord's carrying of password_hash.
type InviteRecord struct {
	Invite
	TokenHash string
}

// LiveInvites returns every invite that has not expired, been redeemed or been revoked as
// of now, hash included.
//
// Why a scan, not a lookup: 09 §5 hashes the invite token with argon2id "exactly like
// a password", and argon2id salts per hash — the same code hashes differently every time,
// so there is no deterministic token_hash to compute from a presented code and match with
// `WHERE token_hash = ?`, the way sessions' SHA-256 allows. A live invite is instead found
// by trying VerifyPassword against each still-live row, stopping at the first match. This
// is deliberately cheap at this project's scale — a friend-group panel outstanding invite
// count is single digits — and is exactly the cost 09 §5 chose when it asked for argon2id
// over a fast hash here (redemption is rare enough to afford it).
func (db *DB) LiveInvites(ctx context.Context, now time.Time) ([]InviteRecord, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(`
		SELECT token_hash, %s FROM invites
		WHERE redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		inviteColumns), FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list live invites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	live := []InviteRecord{}
	for rows.Next() {
		var rec InviteRecord
		inv, err := scanInvite(rows, &rec.TokenHash)
		if err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		rec.Invite = inv
		live = append(live, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list live invites: %w", err)
	}
	return live, nil
}

// RedeemInvite marks id redeemed by userID, at now. A single-use token: the WHERE clause
// only matches a row that is still live, so two concurrent redemptions of the same code
// leave exactly one winner — the second UPDATE affects zero rows.
func (db *DB) RedeemInvite(ctx context.Context, id, userID string, now time.Time) (bool, error) {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE invites SET redeemed_at = ?, redeemed_by = ?
		WHERE id = ? AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		FormatTime(now), userID, id, FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("redeem invite %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("redeem invite %s: %w", id, err)
	}
	return n == 1, nil
}

// RevokeInvite marks id revoked, at now. Revoking an already-redeemed or already-expired
// invite is harmless — Live already says no either way — so this does not check first.
func (db *DB) RevokeInvite(ctx context.Context, id string, now time.Time) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE invites SET revoked_at = ? WHERE id = ?`, FormatTime(now), id); err != nil {
		return fmt.Errorf("revoke invite %s: %w", id, err)
	}
	return nil
}

// ListInvites returns every invite, newest first: an admin-only, unpaginated list.
func (db *DB) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := db.Reader.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM invites ORDER BY created_at DESC`, inviteColumns))
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer func() { _ = rows.Close() }()

	invites := []Invite{}
	for rows.Next() {
		inv, err := scanInvite(rows, nil)
		if err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	return invites, nil
}
