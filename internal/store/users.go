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

// ErrUsernameTaken is returned by CreateUser when the username collides. 04 §2's
// `users.username` is unique; there is no dedicated registry code for it (11 §2.5), so the
// caller renders it as `name_taken` with `details.field: "username"`, the same code
// `instances.name` uses for the same reason.
var ErrUsernameTaken = errors.New("username already exists")

// CountUsers answers whether the panel has ever had an admin — 10 §6's whole bootstrap
// gate turns on this being zero.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts a user already hashed by the caller — this package never sees a
// plaintext password (10 §3.4 is the auth package's job, not the store's).
func (db *DB) CreateUser(ctx context.Context, id, username, passwordHash string, role Role, now time.Time) error {
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, username, passwordHash, string(role), FormatTime(now))
	if isUniqueViolation(err) {
		return ErrUsernameTaken
	}
	if err != nil {
		return fmt.Errorf("create user %s: %w", username, err)
	}
	return nil
}

// ErrBootstrapConsumed means an admin already exists — 10 §6's "no re-bootstrap path".
var ErrBootstrapConsumed = errors.New("bootstrap already consumed")

// CreateFirstAdmin is 10 §6's whole "no re-bootstrap path" guarantee, made real: the
// COUNT(users) check and the insert run inside one transaction on the writer's single
// connection, so two concurrent /setup requests — both past the handler's own pending
// check, both carrying the one valid token — cannot each create an admin. The password is
// hashed by the caller before this is called: argon2id's ~100ms is work, and a transaction
// wraps the state flip, never the work (C1, C2).
func (db *DB) CreateFirstAdmin(ctx context.Context, id, username, passwordHash string, now time.Time) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create first admin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return fmt.Errorf("create first admin: count: %w", err)
	}
	if n != 0 {
		return ErrBootstrapConsumed
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, username, passwordHash, string(RoleAdmin), FormatTime(now))
	if isUniqueViolation(err) {
		return ErrUsernameTaken
	}
	if err != nil {
		return fmt.Errorf("create first admin: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create first admin: commit: %w", err)
	}
	return nil
}

// AuthRecord is the one place password_hash leaves the store — the auth package's own
// verification, never a response (11 §9). It embeds User rather than duplicating its
// fields, so the two cannot drift.
type AuthRecord struct {
	User
	PasswordHash string
}

const userColumns = `id, username, password_hash, role, disabled, created_at, last_login_at`

// scanUser reads one userColumns row from either *sql.Row or *sql.Rows.
func scanUser(s scanner) (AuthRecord, error) {
	var rec AuthRecord
	var lastLogin sql.NullString
	var createdAt string

	err := s.Scan(&rec.ID, &rec.Username, &rec.PasswordHash, &rec.Role, &rec.Disabled, &createdAt, &lastLogin)
	if err != nil {
		return AuthRecord{}, fmt.Errorf("scan user row: %w", err)
	}
	if rec.CreatedAt, err = ParseTime(createdAt); err != nil {
		return AuthRecord{}, fmt.Errorf("created_at: %w", err)
	}
	if lastLogin.Valid {
		t, err := ParseTime(lastLogin.String)
		if err != nil {
			return AuthRecord{}, fmt.Errorf("last_login_at: %w", err)
		}
		rec.LastLoginAt = &t
	}
	return rec, nil
}

// UserForLogin resolves a username to the record login needs to verify against. A missing
// user is not an error: the caller hashes against a dummy either way, so the two cases
// cost the same time (11 §7).
func (db *DB) UserForLogin(ctx context.Context, username string) (*AuthRecord, error) {
	return db.lookupUser(ctx, `username = ?`, username)
}

// UserByID is what every non-login lookup uses — /auth/me, the session middleware, the
// admin user-management endpoints. It returns the same record as UserForLogin; callers
// that must not leak PasswordHash use the embedded User rather than the whole record.
func (db *DB) UserByID(ctx context.Context, id string) (*User, error) {
	rec, err := db.lookupUser(ctx, `id = ?`, id)
	if rec == nil || err != nil {
		return nil, err
	}
	return &rec.User, nil
}

func (db *DB) lookupUser(ctx context.Context, where, arg string) (*AuthRecord, error) {
	row := db.Reader.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM users WHERE %s`, userColumns, where), arg)
	rec, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // "no such user" is the common answer, not a failure
	}
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}
	return &rec, nil
}

// ListUsers returns every user, oldest first — admin-only, unpaginated at M1 scale, same
// as ListInvites (09 §7 puts screens at M5).
func (db *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := db.Reader.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM users ORDER BY created_at`, userColumns))
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	users := []User{}
	for rows.Next() {
		rec, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, rec.User)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// UpdateUserRoleAndDisabled applies a PATCH /users/{id}. Both fields are always written —
// the handler resolves "absent means unchanged" (11 §1.1) against the current row before
// calling this, so the store layer has one unambiguous write rather than a dynamic one.
func (db *DB) UpdateUserRoleAndDisabled(ctx context.Context, id string, role Role, disabled bool) error {
	res, err := db.Writer.ExecContext(ctx,
		`UPDATE users SET role = ?, disabled = ? WHERE id = ?`, string(role), disabled, id)
	if err != nil {
		return fmt.Errorf("update user %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update user %s: %w", id, err)
	} else if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ErrUserNotFound distinguishes a PATCH/DELETE on a missing id from a database error, so
// the handler can answer 404 rather than 500.
var ErrUserNotFound = errors.New("user not found")

// SetUserPassword overwrites a user's hash — self-service change or admin-issued reset
// (09 §5: "no SMTP anywhere", so reset is always admin-issued, never a mailed link).
func (db *DB) SetUserPassword(ctx context.Context, id, passwordHash string) error {
	res, err := db.Writer.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return fmt.Errorf("set password for user %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set password for user %s: %w", id, err)
	} else if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetUserPasswordByUsername is the CLI recovery path (`valmind admin reset`): filesystem
// access to the panel is the correct authentication factor for a root-equivalent panel,
// so this bypasses the API entirely (09 §6).
func (db *DB) SetUserPasswordByUsername(ctx context.Context, username, passwordHash string) error {
	res, err := db.Writer.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE username = ?`, passwordHash, username)
	if err != nil {
		return fmt.Errorf("reset password for %s: %w", username, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("reset password for %s: %w", username, err)
	} else if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// DeleteUser removes a user row. Its sessions cascade (10 §4.1's `ON DELETE CASCADE`); its
// grants and issued invites likewise cascade or null out per 04 §2's own foreign keys.
func (db *DB) DeleteUser(ctx context.Context, id string) error {
	res, err := db.Writer.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	} else if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdateLastLogin stamps last_login_at, the only column a successful login itself writes
// on the users row (the session row carries everything else).
func (db *DB) UpdateLastLogin(ctx context.Context, id string, now time.Time) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`, FormatTime(now), id); err != nil {
		return fmt.Errorf("update last login for user %s: %w", id, err)
	}
	return nil
}

// CreateGrant inserts a per-instance grant — the write side of GrantFor. grantedBy is the
// admin who issued it, or "" for the invite-redemption path where no admin is directly the
// actor at the moment of insert.
func (db *DB) CreateGrant(
	ctx context.Context,
	userID, instanceID string,
	role GrantRole,
	permsJSON, grantedBy string,
	now time.Time,
) error {
	var grantedByArg any
	if grantedBy != "" {
		grantedByArg = grantedBy
	}
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_by, granted_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		userID, instanceID, string(role), permsJSON, grantedByArg, FormatTime(now))
	if err != nil {
		return fmt.Errorf("create grant for user %s on instance %s: %w", userID, instanceID, err)
	}
	return nil
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
