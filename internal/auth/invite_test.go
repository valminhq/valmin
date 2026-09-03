package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func seedInviter(t *testing.T, db *store.DB) string {
	t.Helper()
	seedLoginUser(t, db, "u-admin", "admin", "a-fine-password")
	return "u-admin"
}

func TestIssueAndRedeemInvite(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, 7*24*time.Hour)

	role := store.GrantOperator
	issued, err := invites.Issue(t.Context(), admin, nil, &role, "[]")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Code == "" {
		t.Fatal("Issue returned no code")
	}

	u, inv, err := invites.Redeem(t.Context(), issued.Code, "newbie", "a-fine-password")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "newbie" || u.Role != store.RoleMember {
		t.Errorf("redeemed user = %+v, want a member named newbie", u)
	}
	if inv.ID != issued.Invite.ID {
		t.Errorf("redeemed invite id = %q, want %q", inv.ID, issued.Invite.ID)
	}

	// Registered, not merely a bare struct: this is what the panel treats as an existing
	// account from here on.
	rec, err := db.UserForLogin(t.Context(), "newbie")
	if err != nil || rec == nil {
		t.Fatalf("redeemed user was not persisted: rec=%+v err=%v", rec, err)
	}
}

// TestRedeemAppliesThePreBoundGrant is the regression test for a real bug found while
// wiring the API layer: Redeem created the account but silently dropped the instance and
// role an admin pre-bound to the invite (09 §5, path 2) — "hand the code to the person" is
// meant to grant that access on redemption, not merely record what would have been
// granted.
func TestRedeemAppliesThePreBoundGrant(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	seedGrantableInstance(t, db, "inst-a")
	invites := NewInvites(db, time.Hour)

	instanceID := "inst-a"
	role := store.GrantOperator
	issued, err := invites.Issue(t.Context(), admin, &instanceID, &role, `["backups.create"]`)
	if err != nil {
		t.Fatal(err)
	}

	u, _, err := invites.Redeem(t.Context(), issued.Code, "newbie", "a-fine-password")
	if err != nil {
		t.Fatal(err)
	}

	grant, err := db.GrantFor(t.Context(), u.ID, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if grant == nil {
		t.Fatal("redeeming a pre-bound invite created no grant")
	}
	if grant.Role != store.GrantOperator {
		t.Errorf("grant role = %q, want operator", grant.Role)
	}
	if len(grant.Perms) != 1 || grant.Perms[0] != "backups.create" {
		t.Errorf("grant perms = %v, want [backups.create]", grant.Perms)
	}
}

// TestRedeemWithNoInstanceGrantsNothing: a global invite (no instance_id) must not create
// a stray grant row.
func TestRedeemWithNoInstanceGrantsNothing(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, time.Hour)

	issued, err := invites.Issue(t.Context(), admin, nil, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := invites.Redeem(t.Context(), issued.Code, "newbie", "a-fine-password")
	if err != nil {
		t.Fatal(err)
	}

	ids, err := db.GrantedInstances(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("a global invite granted %v, want nothing", ids)
	}
}

func TestRedeemIsSingleUse(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, 7*24*time.Hour)

	issued, err := invites.Issue(t.Context(), admin, nil, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := invites.Redeem(t.Context(), issued.Code, "first", "a-fine-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invites.Redeem(
		t.Context(),
		issued.Code,
		"second",
		"a-fine-password",
	); !errors.Is(
		err,
		ErrInviteInvalid,
	) {
		t.Errorf("redeeming an already-used code = %v, want ErrInviteInvalid", err)
	}
}

// TestRedeemRejectsExpiredRevokedAndUnknownAlike is 09 §5's "one code, one message": every
// dead reason answers ErrInviteInvalid, so the endpoint cannot be used as an oracle.
func TestRedeemRejectsExpiredRevokedAndUnknownAlike(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, -time.Second) // issues already-expired invites, for this test only

	expired, err := invites.Issue(t.Context(), admin, nil, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := invites.Redeem(
		t.Context(),
		expired.Code,
		"x",
		"a-fine-password",
	); !errors.Is(
		err,
		ErrInviteInvalid,
	) {
		t.Errorf("redeeming an expired invite = %v, want ErrInviteInvalid", err)
	}

	live := NewInvites(db, time.Hour)
	revoked, err := live.Issue(t.Context(), admin, nil, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Revoke(t.Context(), revoked.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.Redeem(t.Context(), revoked.Code, "y", "a-fine-password"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("redeeming a revoked invite = %v, want ErrInviteInvalid", err)
	}

	if _, _, err := live.Redeem(
		t.Context(),
		"totally-made-up-code",
		"z",
		"a-fine-password",
	); !errors.Is(
		err,
		ErrInviteInvalid,
	) {
		t.Errorf("redeeming an unknown code = %v, want ErrInviteInvalid", err)
	}
}

func TestIssueRequiresAnInstanceOrNeither(t *testing.T) {
	// 09 §5: an instance without a role, or a role without an instance, has nothing
	// coherent to grant. That is enforced at the handler's validation layer, not here —
	// this test documents that Issue itself does not enforce it, so the handler must.
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, time.Hour)

	role := store.GrantViewer
	if _, err := invites.Issue(t.Context(), admin, nil, &role, "[]"); err != nil {
		t.Errorf("Issue with a role and no instance should be the handler's problem, not this: %v", err)
	}
}

func TestListInvites(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	admin := seedInviter(t, db)
	invites := NewInvites(db, time.Hour)

	if _, err := invites.Issue(t.Context(), admin, nil, nil, "[]"); err != nil {
		t.Fatal(err)
	}
	if _, err := invites.Issue(t.Context(), admin, nil, nil, "[]"); err != nil {
		t.Fatal(err)
	}

	list, err := invites.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("List = %d invites, want 2", len(list))
	}
}

// TestOldParameterInviteStillVerifies is 10 §3.4: an invite issued before a parameter bump
// still verifies against its own embedded parameters.
func TestOldParameterInviteStillVerifies(t *testing.T) {
	db := testDB(t)
	weak := Argon2Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := db.KVSet(t.Context(), argon2ParamsKey, weak); err != nil {
		t.Fatal(err)
	}
	admin := seedInviter(t, db)
	invites := NewInvites(db, time.Hour)

	issued, err := invites.Issue(t.Context(), admin, nil, nil, "[]")
	if err != nil {
		t.Fatal(err)
	}

	strong := Argon2Params{MemoryKiB: 16 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := db.KVSet(t.Context(), argon2ParamsKey, strong); err != nil {
		t.Fatal(err)
	}

	if _, _, err := invites.Redeem(t.Context(), issued.Code, "newbie", "a-fine-password"); err != nil {
		t.Errorf("an invite issued under old parameters failed to redeem: %v", err)
	}
}
