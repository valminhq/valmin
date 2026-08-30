package store

import (
	"errors"
	"testing"
	"time"
)

func TestCreateAndCountUsers(t *testing.T) {
	db := open(t)

	if n, err := db.CountUsers(t.Context()); err != nil || n != 0 {
		t.Fatalf("CountUsers on an empty panel = %d, %v; want 0, nil", n, err)
	}

	if err := db.CreateUser(t.Context(), "u1", "ada", "hash", RoleAdmin, time.Now()); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if n, err := db.CountUsers(t.Context()); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v; want 1, nil", n, err)
	}
}

// TestCreateUserRejectsADuplicateUsername is the store-level half of 04 §2's
// `users.username` unique constraint: the SQLite error is translated into a sentinel the
// handler can turn into 409 name_taken, rather than a bare 500.
func TestCreateUserRejectsADuplicateUsername(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "hash1", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	err := db.CreateUser(t.Context(), "u2", "ada", "hash2", RoleMember, time.Now())
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("CreateUser on a duplicate username = %v, want ErrUsernameTaken", err)
	}
}

// TestUserForLoginCarriesTheHashUserByIDDoesNot is 11 §9: password_hash never reaches a
// response, and the type system should make that the easy path rather than a discipline.
func TestUserForLoginCarriesTheHashUserByIDDoesNot(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "secret-hash", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}

	rec, err := db.UserForLogin(t.Context(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.PasswordHash != "secret-hash" {
		t.Fatalf("UserForLogin = %+v, want the stored hash", rec)
	}

	u, err := db.UserByID(t.Context(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Username != "ada" {
		t.Fatalf("UserByID = %+v, want ada", u)
	}
}

func TestUserForLoginMissingIsNilNotError(t *testing.T) {
	db := open(t)
	rec, err := db.UserForLogin(t.Context(), "nobody")
	if err != nil {
		t.Fatalf("UserForLogin: %v", err)
	}
	if rec != nil {
		t.Errorf("UserForLogin(nobody) = %+v, want nil", rec)
	}
}

func TestListUsersOrdersOldestFirst(t *testing.T) {
	db := open(t)
	now := time.Now()
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(t.Context(), "u2", "bea", "h", RoleMember, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	users, err := db.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Username != "ada" || users[1].Username != "bea" {
		t.Fatalf("ListUsers = %+v, want ada then bea", users)
	}
}

func TestUpdateUserRoleAndDisabled(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleMember, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUserRoleAndDisabled(t.Context(), "u1", RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	u, err := db.UserByID(t.Context(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleAdmin || !u.Disabled {
		t.Errorf("user = %+v, want admin and disabled", u)
	}

	if err := db.UpdateUserRoleAndDisabled(
		t.Context(),
		"no-such-user",
		RoleAdmin,
		false,
	); !errors.Is(
		err,
		ErrUserNotFound,
	) {
		t.Errorf("update on a missing user = %v, want ErrUserNotFound", err)
	}
}

func TestSetUserPasswordAndByUsername(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "old-hash", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := db.SetUserPassword(t.Context(), "u1", "new-hash"); err != nil {
		t.Fatal(err)
	}
	rec, err := db.UserForLogin(t.Context(), "ada")
	if err != nil || rec.PasswordHash != "new-hash" {
		t.Fatalf("after SetUserPassword: %+v, %v", rec, err)
	}

	// The CLI recovery path (valmind admin reset) resolves by username, with filesystem
	// access standing in for the session a locked-out admin no longer has (09 §6).
	if err := db.SetUserPasswordByUsername(t.Context(), "ada", "cli-hash"); err != nil {
		t.Fatal(err)
	}
	if rec, err := db.UserForLogin(t.Context(), "ada"); err != nil || rec.PasswordHash != "cli-hash" {
		t.Fatalf("after SetUserPasswordByUsername: %+v, %v", rec, err)
	}

	if err := db.SetUserPassword(t.Context(), "no-such-user", "h"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetUserPassword on a missing user = %v, want ErrUserNotFound", err)
	}
	if err := db.SetUserPasswordByUsername(t.Context(), "nobody", "h"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("SetUserPasswordByUsername on a missing user = %v, want ErrUserNotFound", err)
	}
}

// TestDeleteUserCascadesSessions holds 10 §4.1's ON DELETE CASCADE: removing a user must
// not orphan their sessions, or a foreign key check that is only asserted (C9) and never
// exercised is a check nobody has proven works.
func TestDeleteUserCascadesSessions(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.CreateSession(t.Context(), &Session{
		ID: "s1", UserID: "u1", CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour),
	}, "hash1"); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteUser(t.Context(), "u1"); err != nil {
		t.Fatal(err)
	}

	s, u, err := db.SessionAndUser(t.Context(), "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if s != nil || u != nil {
		t.Errorf("session survived its user's deletion: session=%+v user=%+v", s, u)
	}

	if err := db.DeleteUser(t.Context(), "no-such-user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("DeleteUser on a missing user = %v, want ErrUserNotFound", err)
	}
}

func TestCreateGrant(t *testing.T) {
	db := open(t)
	user := seedUser(t, db, "u1")
	instance := seedInstance(t, db, "i1", 2456)
	admin := seedUser(t, db, "u-admin")

	if err := db.CreateGrant(
		t.Context(),
		user,
		instance,
		GrantOperator,
		`["backups.create"]`,
		admin,
		time.Now(),
	); err != nil {
		t.Fatal(err)
	}

	got, err := db.GrantFor(t.Context(), user, instance)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Role != GrantOperator || len(got.Perms) != 1 || got.Perms[0] != "backups.create" {
		t.Errorf("GrantFor = %+v, want operator with backups.create", got)
	}
}

// TestCreateGrantWithNoGrantedBy is the invite-redemption path: the actor at the moment of
// insert is the invite, not a logged-in admin, so granted_by is legitimately absent.
func TestCreateGrantWithNoGrantedBy(t *testing.T) {
	db := open(t)
	user := seedUser(t, db, "u1")
	instance := seedInstance(t, db, "i1", 2456)

	if err := db.CreateGrant(t.Context(), user, instance, GrantViewer, "[]", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := db.GrantFor(t.Context(), user, instance)
	if err != nil || got == nil {
		t.Fatalf("GrantFor: %+v, %v", got, err)
	}
}

func TestUpdateLastLogin(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour)
	if err := db.UpdateLastLogin(t.Context(), "u1", stamp); err != nil {
		t.Fatal(err)
	}
	u, err := db.UserByID(t.Context(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.LastLoginAt == nil || u.LastLoginAt.Unix() != stamp.Unix() {
		t.Errorf("last_login_at = %v, want %v", u.LastLoginAt, stamp)
	}
}
