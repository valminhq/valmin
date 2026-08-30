package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Role is a user's global role (09 §2). A member has no implicit access to anything.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// GrantRole is the base role of a per-instance grant (09 §3.1).
type GrantRole string

const (
	GrantViewer   GrantRole = "viewer"
	GrantOperator GrantRole = "operator"
)

// User is the safe-to-serialize shape of a users row. password_hash and totp_secret are
// deliberately absent: 11 §9 says neither ever appears in a response under any role, and a
// field that is not on the struct cannot be marshalled by accident. Code that needs them
// reads them by their own query.
type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Role        Role       `json:"role"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

// Grant is a live per-instance grant: a base role plus the extra capabilities an admin
// toggled on it (09 §3).
type Grant struct {
	Role  GrantRole
	Perms []string
}

// GrantFor returns the user's grant on instanceID, or nil when there is none.
//
// `↯` The expiry filter is in the SQL, not in the caller, so no call site can forget it.
// An expired grant is no grant, and a column that silently never expires is worse than no
// column (D11, 09 §4). This is the only read of instance_grants that authorizes anything.
func (db *DB) GrantFor(ctx context.Context, userID, instanceID string) (*Grant, error) {
	var role string
	var perms string
	err := db.Reader.QueryRowContext(ctx, `
		SELECT role, perms FROM instance_grants
		WHERE user_id = ? AND instance_id = ?
		  AND (expires_at IS NULL OR expires_at > ?)`,
		userID, instanceID, Now()).Scan(&role, &perms)
	// No grant is the common answer for a member, not a failure.
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read grant for user %s on instance %s: %w", userID, instanceID, err)
	}

	g := &Grant{Role: GrantRole(role)}
	if err := json.Unmarshal([]byte(perms), &g.Perms); err != nil {
		return nil, fmt.Errorf("decode grant perms for user %s on instance %s: %w", userID, instanceID, err)
	}
	return g, nil
}

// GrantedInstances returns the ids of the instances the user holds a live grant on, which
// is exactly what 04 §3's GET /instances may show them. Same expiry filter, same reason.
func (db *DB) GrantedInstances(ctx context.Context, userID string) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT instance_id FROM instance_grants
		WHERE user_id = ?
		  AND (expires_at IS NULL OR expires_at > ?)
		ORDER BY instance_id`, userID, Now())
	if err != nil {
		return nil, fmt.Errorf("list grants for user %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan grant for user %s: %w", userID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list grants for user %s: %w", userID, err)
	}
	return ids, nil
}

// AllInstanceIDs returns every instance id. Only an admin ever sees this list; a member's
// view comes from GrantedInstances (09 §1).
func (db *DB) AllInstanceIDs(ctx context.Context) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT id FROM instances ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan instance id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return ids, nil
}

// InstanceExists reports whether an instance row is present. Callers pair it with an
// authorization decision so an invisible instance and a missing one answer alike (D2).
func (db *DB) InstanceExists(ctx context.Context, instanceID string) (bool, error) {
	var one int
	err := db.Reader.QueryRowContext(ctx,
		`SELECT 1 FROM instances WHERE id = ?`, instanceID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up instance %s: %w", instanceID, err)
	}
	return true, nil
}
