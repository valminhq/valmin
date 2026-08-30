package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func seedLoginUser(t *testing.T, db *store.DB, id, username, password string) {
	t.Helper()
	hash, err := HashPassword(password, fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(t.Context(), id, username, hash, store.RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestLoginSucceedsAndCreatesASession(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")
	sessions := NewSessions(db, time.Hour, 24*time.Hour)

	logged, err := sessions.Login(t.Context(), "ada", "a-fine-password", "203.0.113.7", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if logged.Cookie == "" {
		t.Fatal("Login returned no cookie value")
	}
	if logged.User.Username != "ada" {
		t.Errorf("logged-in user = %+v, want ada", logged.User)
	}
	if logged.SessionID == "" {
		t.Error("Login returned no session id")
	}
	if !logged.AbsoluteExpiresAt.After(time.Now()) {
		t.Errorf("AbsoluteExpiresAt = %v, want a time in the future", logged.AbsoluteExpiresAt)
	}

	got, sessionID, err := sessions.Authenticate(t.Context(), logged.Cookie)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != logged.User.ID {
		t.Errorf("Authenticate = %+v, want the user just logged in", got)
	}
	if sessionID != logged.SessionID {
		t.Errorf("Authenticate session id = %q, want %q", sessionID, logged.SessionID)
	}
}

// TestLoginRejectsAnUnknownUsernameAndAWrongPasswordAlike is 11 §2.5's requirement:
// invalid_credentials, identically, for both — the caller must not be able to tell which
// one it was.
func TestLoginRejectsAnUnknownUsernameAndAWrongPasswordAlike(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")
	sessions := NewSessions(db, time.Hour, 24*time.Hour)

	_, err1 := sessions.Login(t.Context(), "ada", "wrong-password", "", "")
	_, err2 := sessions.Login(t.Context(), "nobody", "whatever-password", "", "")

	if !errors.Is(err1, ErrInvalidCredentials) || !errors.Is(err2, ErrInvalidCredentials) {
		t.Errorf("errors = %v, %v; want ErrInvalidCredentials for both", err1, err2)
	}
}

func TestLoginRejectsADisabledAccount(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")
	if err := db.UpdateUserRoleAndDisabled(t.Context(), "u1", store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessions(db, time.Hour, 24*time.Hour)

	if _, err := sessions.Login(t.Context(), "ada", "a-fine-password", "", ""); !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("Login for a disabled account = %v, want ErrAccountDisabled", err)
	}
}

// TestLoginRehashesAWeakStoredHash is 10 §3.4: on successful login, a hash stored under
// weaker parameters than current is rehashed and stored, without failing the login itself.
func TestLoginRehashesAWeakStoredHash(t *testing.T) {
	db := testDB(t)
	weak := Argon2Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := db.KVSet(t.Context(), argon2ParamsKey, weak); err != nil {
		t.Fatal(err)
	}
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")

	strong := Argon2Params{MemoryKiB: 16 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := db.KVSet(t.Context(), argon2ParamsKey, strong); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessions(db, time.Hour, 24*time.Hour)
	if _, err := sessions.Login(t.Context(), "ada", "a-fine-password", "", ""); err != nil {
		t.Fatal(err)
	}

	rec, err := db.UserForLogin(t.Context(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(rec.PasswordHash, strong) {
		t.Error("the stored hash was not rehashed at the new parameters")
	}
	if !VerifyPassword("a-fine-password", rec.PasswordHash) {
		t.Error("the rehashed value no longer verifies against the same password")
	}
}

func TestAuthenticateRejectsAMalformedCookie(t *testing.T) {
	db := testDB(t)
	sessions := NewSessions(db, time.Hour, 24*time.Hour)
	u, id, err := sessions.Authenticate(t.Context(), "not-a-valid-cookie!!")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u != nil || id != "" {
		t.Errorf("Authenticate(garbage) = %+v, %q, want nil, \"\"", u, id)
	}
}

func TestAuthenticateRejectsADisabledUsersLiveSession(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")
	sessions := NewSessions(db, time.Hour, 24*time.Hour)
	logged, err := sessions.Login(t.Context(), "ada", "a-fine-password", "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Disable out from under the live session, without going through Logout/RevokeAll —
	// 10 §4.1 wants this path covered independently, in case the row survives.
	if err := db.UpdateUserRoleAndDisabled(t.Context(), "u1", store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}

	u, _, err := sessions.Authenticate(t.Context(), logged.Cookie)
	if err != nil {
		t.Fatal(err)
	}
	if u != nil {
		t.Error("a disabled user's still-present session was accepted")
	}
}

func TestLogoutEndsOnlyThatSession(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "a-fine-password")
	sessions := NewSessions(db, time.Hour, 24*time.Hour)

	loggedA, err := sessions.Login(t.Context(), "ada", "a-fine-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	loggedB, err := sessions.Login(t.Context(), "ada", "a-fine-password", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := sessions.Logout(t.Context(), loggedA.SessionID); err != nil {
		t.Fatal(err)
	}

	if u, _, err := sessions.Authenticate(t.Context(), loggedA.Cookie); err != nil || u != nil {
		t.Errorf("logged-out session still authenticates: %+v, %v", u, err)
	}
	if u, _, err := sessions.Authenticate(t.Context(), loggedB.Cookie); err != nil || u == nil {
		t.Errorf("an unrelated session was also ended: %+v, %v", u, err)
	}
}

// TestSetPasswordRevokesEverySession is 10 §4.1's revocation rule reaching a password
// change specifically: every other logged-in copy of this account must stop working.
func TestSetPasswordRevokesEverySession(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	seedLoginUser(t, db, "u1", "ada", "old-password")
	sessions := NewSessions(db, time.Hour, 24*time.Hour)

	logged, err := sessions.Login(t.Context(), "ada", "old-password", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := sessions.SetPassword(t.Context(), "u1", "new-password"); err != nil {
		t.Fatal(err)
	}

	if u, _, err := sessions.Authenticate(t.Context(), logged.Cookie); err != nil || u != nil {
		t.Errorf("session survived a password change: %+v, %v", u, err)
	}
	if _, err := sessions.Login(t.Context(), "ada", "old-password", "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	if _, err := sessions.Login(t.Context(), "ada", "new-password", "", ""); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}
