package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// ErrInvalidCredentials covers a wrong username and a wrong password identically — 11 §2.5
// requires the two be indistinguishable, in both response and timing (11 §7).
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrAccountDisabled is 09's disabled flag reaching login. It is a distinct error from
// ErrInvalidCredentials only for the log line; the caller renders both the same way, or
// a disabled account becomes an oracle for "this username exists and is disabled".
var ErrAccountDisabled = errors.New("account disabled")

// Sessions owns login, session validation and logout (10 §4.1).
type Sessions struct {
	db       *store.DB
	idleTTL  time.Duration
	absTTL   time.Duration
	throttle time.Duration
	revoked  func(sessionID, userID string)
}

// OnRevoke registers what to do about live connections when a session stops being valid
// (10 §4.1, 14 §6). Deleting the row stops the next request; a WebSocket makes no next
// request, so an admin who revokes access and watches the UI update has every reason to
// believe it is gone while the revoked user's console keeps streaming.
//
// Exactly one of sessionID and userID is set: the first for a single logout, the second
// for everything that invalidates the whole account. Set at wiring, before anything
// serves.
func (s *Sessions) OnRevoke(fn func(sessionID, userID string)) { s.revoked = fn }

func (s *Sessions) notifyRevoked(sessionID, userID string) {
	if s.revoked != nil {
		s.revoked(sessionID, userID)
	}
}

// touchThrottle is 10 §4.1's own number: a live WebSocket must not hammer the single
// writer with a last_seen_at update on every frame.
const touchThrottle = time.Minute

func NewSessions(db *store.DB, idleTTL, absoluteTTL time.Duration) *Sessions {
	return &Sessions{db: db, idleTTL: idleTTL, absTTL: absoluteTTL, throttle: touchThrottle}
}

// LoggedIn is what a successful Login (or an auto-login after setup or invite redemption)
// hands back — everything the HTTP layer needs to set the cookie and derive the CSRF
// token bound to it (11 §6.2 needs the session id, not the cookie value).
type LoggedIn struct {
	Cookie            string
	SessionID         string
	User              store.User
	AbsoluteExpiresAt time.Time
}

// Login verifies a username and password and, on success, creates a session. ip and
// userAgent are stored on the row for the operator's own audit trail, never rendered back
// to a caller with a lesser view.
//
// `↯` An unknown username still pays the argon2id cost, against a fixed dummy hash — the
// timing difference between "no such user" and "wrong password" is exactly the oracle
// 11 §7 requires be closed.
func (s *Sessions) Login(ctx context.Context, username, password, ip, userAgent string) (*LoggedIn, error) {
	rec, err := s.db.UserForLogin(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}
	if rec == nil {
		VerifyAgainstDummy(password)
		return nil, ErrInvalidCredentials
	}
	if !VerifyPassword(password, rec.PasswordHash) {
		return nil, ErrInvalidCredentials
	}
	if rec.Disabled {
		return nil, ErrAccountDisabled
	}

	if params, perr := LoadArgon2Params(ctx, s.db); perr == nil && NeedsRehash(rec.PasswordHash, params) {
		// 10 §3.4: rehash on successful login when stored parameters are below current.
		// Best-effort — a failure here does not fail the login the user is waiting on.
		if newHash, herr := HashPassword(password, params); herr == nil {
			_ = s.db.SetUserPassword(ctx, rec.ID, newHash)
		}
	}

	now := time.Now()
	cookieValue, tokenHash := NewSessionToken()
	sess := &store.Session{
		ID: store.NewID(), UserID: rec.ID, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(s.idleTTL), AbsoluteExpiresAt: now.Add(s.absTTL),
		IP: ip, UserAgent: userAgent,
	}
	if err := s.db.CreateSession(ctx, sess, tokenHash); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if err := s.db.UpdateLastLogin(ctx, rec.ID, now); err != nil {
		return nil, fmt.Errorf("update last login: %w", err)
	}

	return &LoggedIn{
		Cookie: cookieValue, SessionID: sess.ID, User: rec.User, AbsoluteExpiresAt: sess.AbsoluteExpiresAt,
	}, nil
}

// Authenticate resolves a cookie value to the user and session id it belongs to. It
// satisfies middleware.SessionAuthenticator, defined by that package as its consumer
// (06 §4) — this package has no HTTP dependency.
//
// A disabled user is rejected here too, not only at Login: 10 §4.1's revocation is meant
// to reach a live session, and deleting the row is what does that, but a disabled account
// whose session happens to survive (a bug elsewhere) must not be trusted regardless.
func (s *Sessions) Authenticate(ctx context.Context, cookieValue string) (*store.User, string, error) {
	tokenHash, ok := tryHashSessionToken(cookieValue)
	if !ok {
		return nil, "", nil
	}
	sess, u, err := s.db.SessionAndUser(ctx, tokenHash)
	if err != nil {
		return nil, "", fmt.Errorf("look up session: %w", err)
	}
	if sess == nil || u.Disabled {
		return nil, "", nil
	}

	// Throttled per 10 §4.1: at most one write per touchThrottle, so a live socket
	// polling this on every frame does not hammer the single writer.
	if err := s.db.TouchSession(ctx, sess.ID, time.Now(), s.idleTTL, s.throttle); err != nil {
		return nil, "", fmt.Errorf("touch session: %w", err)
	}
	return u, sess.ID, nil
}

// Logout deletes the one session the caller is using.
func (s *Sessions) Logout(ctx context.Context, sessionID string) error {
	if err := s.db.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	s.notifyRevoked(sessionID, "")
	return nil
}

// RevokeAll deletes every session belonging to userID — password change, role change and
// disabling all reach live connections this way (10 §4.1).
func (s *Sessions) RevokeAll(ctx context.Context, userID string) error {
	if err := s.db.DeleteSessionsForUser(ctx, userID); err != nil {
		return fmt.Errorf("delete sessions for user %s: %w", userID, err)
	}
	s.notifyRevoked("", userID)
	return nil
}

// SetPassword hashes and stores a new password, then revokes every other session — a
// password change is exactly the moment every other logged-in copy of this account should
// stop being trusted.
func (s *Sessions) SetPassword(ctx context.Context, userID, password string) error {
	params, err := LoadArgon2Params(ctx, s.db)
	if err != nil {
		return err
	}
	hash, err := HashPassword(password, params)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.db.SetUserPassword(ctx, userID, hash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return s.RevokeAll(ctx, userID)
}
