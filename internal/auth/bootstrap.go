package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// setupTokenTTL is 10 §6's own number — a panel left running for a week must not still
// accept a token printed on Monday. Not configurable: there is no `10 §1.1` key for it,
// and a knob here is a knob for weakening it.
const setupTokenTTL = 15 * time.Minute

const bootstrapStateKey = "bootstrap_state"

// bootstrapState is kv["bootstrap_state"] (10 §6). Pending is carried alongside
// COUNT(users) rather than instead of it — COUNT(users) is the fact that can never drift,
// this is only where the current token's hash and expiry live.
type bootstrapState struct {
	Pending   bool      `json:"pending"`
	TokenHash string    `json:"token_hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrSetupConsumed is returned once an admin already exists — 10 §6: no re-bootstrap path.
var ErrSetupConsumed = errors.New("setup already consumed")

// ErrSetupTokenInvalid covers a wrong, expired or already-superseded token. There is only
// one reason given to the caller regardless of which — the token is 32 bytes of CSPRNG
// printed to a log an attacker should not have; there is no oracle worth closing here the
// way there is for invites, but there is also no reason to distinguish the cases.
var ErrSetupTokenInvalid = errors.New("setup token invalid or expired")

// Bootstrap owns 10 §6's first-run flow.
type Bootstrap struct {
	db *store.DB
}

func NewBootstrap(db *store.DB) *Bootstrap { return &Bootstrap{db: db} }

// Pending reports whether the panel still needs its first admin. This is the one
// authoritative source — COUNT(users) — never the kv flag, which cannot drift from it by
// construction. Callers that need this on every request should cache it in memory and
// invalidate on Setup succeeding, not call this per request (11 §5.3).
func (b *Bootstrap) Pending(ctx context.Context) (bool, error) {
	n, err := b.db.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("check bootstrap state: %w", err)
	}
	return n == 0, nil
}

// PrintToken generates a fresh token and prints it framed to w, if the panel is still
// pending. Called once per process start (10 §6): "regenerated on each restart while
// unconsumed" — not on a timer, so a token from an hour-old process is only ever refreshed
// by restarting it.
func (b *Bootstrap) PrintToken(ctx context.Context, w io.Writer) error {
	pending, err := b.Pending(ctx)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}

	token, hash := NewSetupToken()
	state := bootstrapState{Pending: true, TokenHash: hash, ExpiresAt: time.Now().Add(setupTokenTTL)}
	if err := b.db.KVSet(ctx, bootstrapStateKey, state); err != nil {
		return fmt.Errorf("store bootstrap state: %w", err)
	}

	const rule = "============================================================"
	_, werr := fmt.Fprintf(w, "\n%s\n"+
		"  Valmin first-run setup\n"+
		"  This panel has no administrator yet. Open the panel and enter this token\n"+
		"  within %s to create one:\n\n"+
		"      %s\n\n"+
		"  A new token is printed on every restart until an administrator exists.\n"+
		"%s\n\n", rule, setupTokenTTL, token, rule)
	if werr != nil {
		return fmt.Errorf("print setup token: %w", werr)
	}
	return nil
}

// Setup verifies token and creates the first admin. It never distinguishes "wrong token"
// from "already consumed" in its error beyond the two sentinels above, and D10 applies
// just as it does to a login: the token itself never appears in a log line here.
//
// The pending check here is a courtesy — a fast, obvious rejection before the (~100ms)
// argon2id hash runs. The guarantee that only one admin is ever created is
// store.CreateFirstAdmin's, which re-checks atomically on the writer's single connection;
// two concurrent requests carrying the one valid token cannot both win.
func (b *Bootstrap) Setup(ctx context.Context, token, username, password string) (*store.User, error) {
	pending, err := b.Pending(ctx)
	if err != nil {
		return nil, err
	}
	if !pending {
		return nil, ErrSetupConsumed
	}

	var state bootstrapState
	found, err := b.db.KVGet(ctx, bootstrapStateKey, &state)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap state: %w", err)
	}
	gotHash, err := HashSessionToken(token)
	if !found || err != nil || state.TokenHash != gotHash || time.Now().After(state.ExpiresAt) {
		return nil, ErrSetupTokenInvalid
	}

	params, err := LoadArgon2Params(ctx, b.db)
	if err != nil {
		return nil, err
	}
	hash, err := HashPassword(password, params)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	now := time.Now()
	id := store.NewID()
	if err := b.db.CreateFirstAdmin(ctx, id, username, hash, now); err != nil {
		if errors.Is(err, store.ErrBootstrapConsumed) {
			return nil, ErrSetupConsumed
		}
		return nil, fmt.Errorf("create first admin: %w", err)
	}
	if err := b.db.KVSet(ctx, bootstrapStateKey, bootstrapState{Pending: false}); err != nil {
		return nil, fmt.Errorf("clear bootstrap state: %w", err)
	}
	return &store.User{ID: id, Username: username, Role: store.RoleAdmin, CreatedAt: now}, nil
}
