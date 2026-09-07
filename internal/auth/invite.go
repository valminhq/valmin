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
// The audit row records who issued what, and from which client IP, and never the token.
func (inv *Invites) Issue(
	ctx context.Context, createdBy string, instanceID *string, role *store.GrantRole, permsJSON, ip string,
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
	if err := json.Unmarshal([]byte(permsJSON), &rec.GrantPerms); err != nil {
		return nil, fmt.Errorf("decode invite perms: %w", err)
	}
	detail, err := json.Marshal(struct {
		InviteID  string           `json:"invite_id"`
		Instance  *string          `json:"instance_id"`
		GrantRole *store.GrantRole `json:"grant_role"`
		Perms     []string         `json:"grant_perms"`
		ExpiresAt time.Time        `json:"expires_at"`
	}{
		InviteID: rec.ID, Instance: instanceID, GrantRole: role,
		Perms: rec.GrantPerms, ExpiresAt: rec.ExpiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("encode invite audit detail: %w", err)
	}
	if err := inv.db.CreateInviteAudited(ctx, &rec, hash, permsJSON, &store.AuditEntry{
		UserID: createdBy, Action: "invites.issue", Detail: string(detail), IP: ip,
	}); err != nil {
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
func (inv *Invites) Redeem(
	ctx context.Context, code, username, password, ip string,
) (*store.User, *store.Invite, error) {
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
	detail, err := json.Marshal(map[string]string{"invite_id": matched.ID, "target_user_id": userID})
	if err != nil {
		return nil, nil, fmt.Errorf("encode invite redemption audit detail: %w", err)
	}
	audit := &store.AuditEntry{
		UserID: userID, Action: "invites.redeem", Detail: string(detail), IP: ip,
	}
	if matched.InstanceID != nil {
		audit.InstanceID = *matched.InstanceID
	}
	if err := inv.db.RedeemInviteToUser(ctx, &matched.Invite, userID, username, hash, now, audit); err != nil {
		if errors.Is(err, store.ErrInviteNotLive) {
			return nil, nil, ErrInviteInvalid
		}
		return nil, nil, fmt.Errorf("redeem invite: %w", err)
	}

	u := &store.User{ID: userID, Username: username, Role: store.RoleMember, CreatedAt: now}
	return u, &matched.Invite, nil
}

// Revoke marks an invite dead. Revoking one that is already dead is a no-op, not an error
// — 09 §5's own liveness check already treats it as gone either way.
func (inv *Invites) Revoke(ctx context.Context, id, actorID, ip string) error {
	detail, err := json.Marshal(map[string]string{"invite_id": id})
	if err != nil {
		return fmt.Errorf("encode invite revocation audit detail: %w", err)
	}
	if err := inv.db.RevokeInviteAudited(ctx, id, time.Now(), &store.AuditEntry{
		UserID: actorID, Action: "invites.revoke", Detail: string(detail), IP: ip,
	}); err != nil {
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
