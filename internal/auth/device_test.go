package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// TestDeviceTrustExpiresAndFollowsThePassword asserts that a trusted-browser token is accepted
// for its own account until it expires or the password changes, and never for another account.
func TestDeviceTrustExpiresAndFollowsThePassword(t *testing.T) {
	db := testDB(t)
	for _, u := range []struct{ id, name string }{{"u1", "ada"}, {"u2", "bob"}} {
		if err := db.CreateUser(t.Context(), u.id, u.name, "hash-"+u.id, store.RoleAdmin, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte("test key")
	now := time.Now()
	d := NewDeviceTrust(db, func(msg []byte) ([]byte, error) {
		m := hmac.New(sha256.New, key)
		m.Write(msg)
		return m.Sum(nil), nil
	})
	d.now = func() time.Time { return now }

	token, err := d.Token(t.Context(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Trusted(t.Context(), "ada", token) {
		t.Fatal("a fresh token is not trusted")
	}
	if d.Trusted(t.Context(), "bob", token) {
		t.Error("ada's token is trusted for bob")
	}

	now = now.Add(DeviceTrustAge + time.Minute)
	if d.Trusted(t.Context(), "ada", token) {
		t.Error("a token past its age is trusted")
	}

	now = time.Now()
	token, err = d.Token(t.Context(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserPassword(t.Context(), "u1", "hash-new"); err != nil {
		t.Fatal(err)
	}
	if d.Trusted(t.Context(), "ada", token) {
		t.Error("a token from before a password change is trusted")
	}
}
