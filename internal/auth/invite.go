package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// ErrInviteInvalid is 09 §5's one answer for expired, revoked, redeemed and never-existed
// alike — one code, one message, or the endpoint is a token oracle.
var ErrInviteInvalid = errors.New("invite invalid")

// Invites owns issuing and redeeming invite tokens (09 §5).
type Invites struct {
	db  *store.DB
	ttl time.Duration
}

func NewInvites(db *store.DB, ttl time.Duration) *Invites { return &Invites{db: db, ttl: ttl} }

// Issued is what the admin sees once, at creation — the plaintext code never exists again
// after this response (09 §5).
type Issued struct {
	Code      string
	ExpiresAt time.Time
	Invite    store.Invite
}

// Issue creates an invite. instanceID and role are both optional, but an instance without
// a role — or a role without an instance — has nothing to grant, so both or neither.
func (inv *Invites) Issue(
	ctx context.Context, createdBy string, instanceID *string, role *store.GrantRole, permsJSON string,
) (*Issued, error) {
	params, err := LoadArgon2Params(ctx, inv.db)
	if err != nil {
		return nil, err
	}
	code, hash, err := NewInviteToken(params)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	rec := store.Invite{
		ID: store.NewID(), CreatedBy: createdBy, InstanceID: instanceID, GrantRole: role,
		ExpiresAt: now.Add(inv.ttl), CreatedAt: now,
	}
	if err := inv.db.CreateInvite(ctx, &rec, hash, permsJSON); err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	return &Issued{Code: code, ExpiresAt: rec.ExpiresAt, Invite: rec}, nil
}

// Redeem verifies code and, if it is still live, creates the account it names. Expired,
// revoked, redeemed and never-existed all return the same ErrInviteInvalid, so the endpoint
// cannot be used to tell them apart (09 §5).
//
// code is matched by trying VerifyPassword against every currently-live invite rather than a
// hash lookup, since argon2id salts per hash and there is no deterministic token_hash to match
// (store.LiveInvites). Cheap at a friend-group panel's scale.
func (inv *Invites) Redeem(ctx context.Context, code, username, password string) (*store.User, *store.Invite, error) {
	live, err := inv.db.LiveInvites(ctx, time.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("list live invites: %w", err)
	}
	var matched *store.InviteRecord
	for i := range live {
		if VerifyPassword(code, live[i].TokenHash) {
			matched = &live[i]
			break
		}
	}
	if matched == nil {
		return nil, nil, ErrInviteInvalid
	}

	params, err := LoadArgon2Params(ctx, inv.db)
	if err != nil {
		return nil, nil, err
	}
	hash, err := HashPassword(password, params)
	if err != nil {
		return nil, nil, fmt.Errorf("hash password: %w", err)
	}
	now := time.Now()
	userID := store.NewID()
	if err := inv.db.CreateUser(ctx, userID, username, hash, store.RoleMember, now); err != nil {
		return nil, nil, fmt.Errorf("create user from invite: %w", err)
	}

	ok, err := inv.db.RedeemInvite(ctx, matched.ID, userID, now)
	if err != nil {
		return nil, nil, fmt.Errorf("redeem invite: %w", err)
	}
	if !ok {
		// Lost the race to a concurrent redemption of the same code. The account already exists
		// with no grant attached, which is acceptable: 09 §5 promises only that the invite is
		// single-use, not atomicity between account and grant.
		return nil, nil, ErrInviteInvalid
	}

	// Redeeming an invite pre-bound to an instance and role must actually grant that access
	// (09 §5 path 2). GrantPerms was already validated as grantable at issue time
	// (Invites.validateIssue), so it is re-encoded here rather than re-checked.
	if matched.InstanceID != nil {
		permsJSON, err := json.Marshal(matched.GrantPerms)
		if err != nil {
			return nil, nil, fmt.Errorf("encode grant perms: %w", err)
		}
		if err := inv.db.CreateGrant(
			ctx, userID, *matched.InstanceID, *matched.GrantRole, string(permsJSON), matched.CreatedBy, now,
		); err != nil {
			return nil, nil, fmt.Errorf("apply invite grant: %w", err)
		}
	}

	u := &store.User{ID: userID, Username: username, Role: store.RoleMember, CreatedAt: now}
	return u, &matched.Invite, nil
}

// Revoke marks an invite dead. Revoking one that is already dead is a no-op, not an error
// — 09 §5's own liveness check already treats it as gone either way.
func (inv *Invites) Revoke(ctx context.Context, id string) error {
	if err := inv.db.RevokeInvite(ctx, id, time.Now()); err != nil {
		return fmt.Errorf("revoke invite %s: %w", id, err)
	}
	return nil
}

// List returns every invite, for the admin-only view.
func (inv *Invites) List(ctx context.Context) ([]store.Invite, error) {
	list, err := inv.db.ListInvites(ctx)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	return list, nil
}
