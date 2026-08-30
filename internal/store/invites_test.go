package store

import (
	"testing"
	"time"
)

// seedAdminAndInstance seeds the rows invites.created_by and invites.instance_id
// reference — foreign_keys is asserted on (C9), so an invite naming either without the
// row existing is rejected rather than silently accepted.
func seedAdminAndInstance(t *testing.T, db *DB) {
	t.Helper()
	exec(t, db.Writer,
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		"u-admin", "admin", "h", string(RoleAdmin), Now())
	seedInstance(t, db, "inst-a", 2456)
}

// newInvite creates an invite and returns its id and the (opaque, test-only) hash string
// stored for it. The store layer never hashes anything itself — the caller in the real
// system is the auth package's argon2id, and here a plain string stands in for it.
func newInvite(t *testing.T, db *DB, id string, expiresAt time.Time) (tokenHash string) {
	t.Helper()
	tokenHash = "hash-" + id
	role := GrantOperator
	instanceID := "inst-a"
	if err := db.CreateInvite(t.Context(), &Invite{
		ID: id, CreatedBy: "u-admin", InstanceID: &instanceID, GrantRole: &role,
		ExpiresAt: expiresAt, CreatedAt: time.Now(),
	}, tokenHash, `["backups.create"]`); err != nil {
		t.Fatal(err)
	}
	return tokenHash
}

func TestInviteRoundTrips(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	future := time.Now().Add(7 * 24 * time.Hour)
	newInvite(t, db, "inv1", future)

	inv, err := db.InviteByID(t.Context(), "inv1")
	if err != nil {
		t.Fatal(err)
	}
	if inv == nil {
		t.Fatal("InviteByID returned nil for a live invite")
	}
	if inv.InstanceID == nil || *inv.InstanceID != "inst-a" {
		t.Errorf("instance_id = %v, want inst-a", inv.InstanceID)
	}
	if inv.GrantRole == nil || *inv.GrantRole != GrantOperator {
		t.Errorf("grant_role = %v, want operator", inv.GrantRole)
	}
	if len(inv.GrantPerms) != 1 || inv.GrantPerms[0] != "backups.create" {
		t.Errorf("grant_perms = %v, want [backups.create]", inv.GrantPerms)
	}
	if !inv.Live(time.Now()) {
		t.Error("a fresh invite reports itself as not live")
	}
}

func TestInviteByIDMissingIsNilNotError(t *testing.T) {
	db := open(t)
	inv, err := db.InviteByID(t.Context(), "no-such-id")
	if err != nil {
		t.Fatalf("InviteByID: %v", err)
	}
	if inv != nil {
		t.Errorf("InviteByID(missing) = %+v, want nil", inv)
	}
}

// TestInviteLivenessCoversEveryDeadCase is the SQL half of 09 §5's "one code, one message"
// requirement: whatever the reason an invite cannot be redeemed, Live must say so, so the
// handler answers invite_invalid uniformly rather than leaking which reason applied.
func TestInviteLivenessCoversEveryDeadCase(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	now := time.Now()

	newInvite(t, db, "inv-expired", now.Add(-time.Second))
	newInvite(t, db, "inv-revoked", now.Add(time.Hour))
	newInvite(t, db, "inv-redeemed", now.Add(time.Hour))

	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleMember, now); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeInvite(t.Context(), "inv-revoked", now); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RedeemInvite(t.Context(), "inv-redeemed", "u1", now); err != nil || !ok {
		t.Fatalf("seeding a redeemed invite: ok=%v err=%v", ok, err)
	}

	for _, id := range []string{"inv-expired", "inv-revoked", "inv-redeemed"} {
		inv, err := db.InviteByID(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if inv == nil {
			t.Fatalf("invite %s vanished instead of reporting as dead", id)
		}
		if inv.Live(now) {
			t.Errorf("invite %s reports Live, want dead", id)
		}
	}
}

// TestLiveInvitesExcludesDeadOnes is the store half of the argon2id scan-and-verify
// lookup 09 §5 requires (Redeem in the auth package): LiveInvites must not hand back a
// row that Live would say no to, or the scan would find and accept a dead invite.
func TestLiveInvitesExcludesDeadOnes(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	now := time.Now()

	liveHash := newInvite(t, db, "inv-live", now.Add(time.Hour))
	newInvite(t, db, "inv-expired", now.Add(-time.Second))
	newInvite(t, db, "inv-revoked", now.Add(time.Hour))

	if err := db.RevokeInvite(t.Context(), "inv-revoked", now); err != nil {
		t.Fatal(err)
	}

	live, err := db.LiveInvites(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("LiveInvites = %+v, want exactly the one live invite", live)
	}
	if live[0].ID != "inv-live" || live[0].TokenHash != liveHash {
		t.Errorf("LiveInvites[0] = %+v, want inv-live with its hash", live[0])
	}
}

// TestRedeemInviteIsSingleUse: two concurrent redemptions of the same code must leave
// exactly one winner, enforced by the WHERE clause rather than a read-then-write race.
func TestRedeemInviteIsSingleUse(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	now := time.Now()
	newInvite(t, db, "inv1", now.Add(time.Hour))
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", RoleMember, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(t.Context(), "u2", "bea", "h", RoleMember, now); err != nil {
		t.Fatal(err)
	}

	first, err := db.RedeemInvite(t.Context(), "inv1", "u1", now)
	if err != nil || !first {
		t.Fatalf("first redemption: ok=%v err=%v", first, err)
	}
	second, err := db.RedeemInvite(t.Context(), "inv1", "u2", now)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a redeemed invite was redeemed a second time")
	}

	got, err := db.InviteByID(t.Context(), "inv1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RedeemedBy == nil || *got.RedeemedBy != "u1" {
		t.Errorf("redeemed_by = %v, want u1 (the first winner)", got.RedeemedBy)
	}
}

func TestListInvitesOrdersNewestFirst(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	now := time.Now()
	newInvite(t, db, "inv-old", now.Add(time.Hour))
	// A distinguishable created_at: list order is by created_at, not insertion order.
	if err := db.CreateInvite(t.Context(), &Invite{
		ID: "inv-new", CreatedBy: "u-admin", ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(time.Minute),
	}, "hash-inv-new", "[]"); err != nil {
		t.Fatal(err)
	}

	invites, err := db.ListInvites(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 2 || invites[0].ID != "inv-new" || invites[1].ID != "inv-old" {
		t.Fatalf("ListInvites = %+v, want inv-new then inv-old", invites)
	}
}

func TestInviteWithNoInstanceIsGlobal(t *testing.T) {
	db := open(t)
	seedAdminAndInstance(t, db)
	if err := db.CreateInvite(t.Context(), &Invite{
		ID: "inv1", CreatedBy: "u-admin", ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}, "hash1", "[]"); err != nil {
		t.Fatal(err)
	}
	inv, err := db.InviteByID(t.Context(), "inv1")
	if err != nil {
		t.Fatal(err)
	}
	if inv.InstanceID != nil || inv.GrantRole != nil {
		t.Errorf("invite = %+v, want no instance and no grant role", inv)
	}
}
