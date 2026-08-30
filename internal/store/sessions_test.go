package store

import (
	"testing"
	"time"
)

func newSession(t *testing.T, db *DB, id, userID string, now time.Time, idle, absolute time.Duration) string {
	t.Helper()
	tokenHash := "hash-" + id
	if err := db.CreateSession(t.Context(), &Session{
		ID: id, UserID: userID, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(idle), AbsoluteExpiresAt: now.Add(absolute),
		IP: "203.0.113.7", UserAgent: "test-agent",
	}, tokenHash); err != nil {
		t.Fatal(err)
	}
	return tokenHash
}

func TestSessionAndUserRoundTrips(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hash := newSession(t, db, "s1", "u1", now, time.Hour, 24*time.Hour)

	s, u, err := db.SessionAndUser(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.ID != "s1" || s.IP != "203.0.113.7" {
		t.Fatalf("session = %+v, want s1", s)
	}
	if u == nil || u.Username != "ada" {
		t.Fatalf("user = %+v, want ada", u)
	}
}

func TestSessionAndUserMissingIsNilNotError(t *testing.T) {
	db := open(t)
	s, u, err := db.SessionAndUser(t.Context(), "no-such-hash")
	if err != nil {
		t.Fatalf("SessionAndUser: %v", err)
	}
	if s != nil || u != nil {
		t.Errorf("SessionAndUser(missing) = %+v, %+v, want nil, nil", s, u)
	}
}

// TestExpiredSessionIsRejected is D16's model half: a session past either expiry is no
// session, filtered in the SQL the same way instance_grants.expires_at is (D11).
func TestExpiredSessionIsRejected(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	tests := []struct {
		name     string
		idle     time.Duration
		absolute time.Duration
	}{
		{name: "idle expired", idle: -time.Second, absolute: 24 * time.Hour},
		{name: "absolute expired", idle: time.Hour, absolute: -time.Second},
		{name: "both expired", idle: -time.Hour, absolute: -time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash := newSession(t, db, "s-"+tt.name, "u1", now, tt.idle, tt.absolute)
			s, u, err := db.SessionAndUser(t.Context(), hash)
			if err != nil {
				t.Fatal(err)
			}
			if s != nil || u != nil {
				t.Errorf("expired session was returned: %+v, %+v", s, u)
			}
		})
	}
}

// TestSessionPastAbsoluteExpiryRejectedEvenWithFreshLastSeen is the WP-09 acceptance
// criterion verbatim: last_seen_at being current must not paper over an absolute expiry
// that has already passed — the two expiries are independent, and absolute never extends.
func TestSessionPastAbsoluteExpiryRejectedEvenWithFreshLastSeen(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hash := newSession(t, db, "s1", "u1", now, time.Hour, -time.Second)

	// Touch first, as a live socket would: last_seen_at becomes "now", well inside the
	// idle window, but the absolute deadline already passed at creation.
	if err := db.TouchSession(t.Context(), "s1", now, time.Hour, 0); err != nil {
		t.Fatal(err)
	}

	s, u, err := db.SessionAndUser(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil || u != nil {
		t.Error("a session past its absolute expiry was accepted because last_seen_at was current")
	}
}

// TestTouchSessionIsThrottled is the other acceptance criterion: last_seen_at is written
// at most once per throttle window, so a live socket does not hammer the single writer.
func TestTouchSessionIsThrottled(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	newSession(t, db, "s1", "u1", start, time.Hour, 24*time.Hour)

	const throttle = time.Minute
	if err := db.TouchSession(t.Context(), "s1", start.Add(10*time.Second), time.Hour, throttle); err != nil {
		t.Fatal(err)
	}
	afterFirst, err := sessionRow(t, db, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !afterFirst.LastSeenAt.Equal(start.UTC()) {
		t.Errorf("a touch inside the throttle window updated last_seen_at: %v", afterFirst.LastSeenAt)
	}

	if err := db.TouchSession(t.Context(), "s1", start.Add(90*time.Second), time.Hour, throttle); err != nil {
		t.Fatal(err)
	}
	afterSecond, err := sessionRow(t, db, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if afterSecond.LastSeenAt.Equal(start.UTC()) {
		t.Error("a touch past the throttle window did not update last_seen_at")
	}
}

// sessionRow reads a session by id directly, bypassing the liveness filter SessionAndUser
// applies, so a test can inspect last_seen_at without the row also having to be live.
func sessionRow(t *testing.T, db *DB, id string) (Session, error) {
	t.Helper()
	var s Session
	var createdAt, lastSeen, idleExp, absExp string
	err := db.Reader.QueryRowContext(t.Context(), `
		SELECT id, user_id, created_at, last_seen_at, idle_expires_at, absolute_expires_at
		FROM sessions WHERE id = ?`, id).Scan(&s.ID, &s.UserID, &createdAt, &lastSeen, &idleExp, &absExp)
	if err != nil {
		return Session{}, err
	}
	s.LastSeenAt, err = ParseTime(lastSeen)
	return s, err
}

func TestDeleteSession(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hash := newSession(t, db, "s1", "u1", now, time.Hour, 24*time.Hour)

	if err := db.DeleteSession(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	s, u, err := db.SessionAndUser(t.Context(), hash)
	if err != nil || s != nil || u != nil {
		t.Errorf("session survived DeleteSession: %+v %+v %v", s, u, err)
	}
}

// TestDeleteSessionsForUserReachesEveryConnection is 10 §4.1's revocation rule: password
// change, role change and disabling a user must invalidate every session at once, not just
// the one the request arrived on.
func TestDeleteSessionsForUserReachesEveryConnection(t *testing.T) {
	db := open(t)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(t.Context(), "u2", "bea", "h", RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h1 := newSession(t, db, "s1", "u1", now, time.Hour, 24*time.Hour)
	h2 := newSession(t, db, "s2", "u1", now, time.Hour, 24*time.Hour)
	h3 := newSession(t, db, "s3", "u2", now, time.Hour, 24*time.Hour)

	if err := db.DeleteSessionsForUser(t.Context(), "u1"); err != nil {
		t.Fatal(err)
	}

	for _, hash := range []string{h1, h2} {
		if s, _, _ := db.SessionAndUser(t.Context(), hash); s != nil {
			t.Errorf("session %s survived DeleteSessionsForUser", hash)
		}
	}
	if s, _, err := db.SessionAndUser(t.Context(), h3); err != nil || s == nil {
		t.Errorf("an unrelated user's session was also revoked: %+v, %v", s, err)
	}
}
