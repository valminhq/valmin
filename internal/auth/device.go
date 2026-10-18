package auth

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/store"
)

// DeviceTrust signs and checks the value of the cookie that marks a browser where an account
// has signed in. The signature covers the account's id and current password hash, so changing
// or resetting the password withdraws every browser's trust at once.
type DeviceTrust struct {
	db  *store.DB
	mac func(msg []byte) ([]byte, error)
}

// NewDeviceTrust signs with mac, which must be a keyed MAC the panel holds alone.
func NewDeviceTrust(db *store.DB, mac func(msg []byte) ([]byte, error)) *DeviceTrust {
	return &DeviceTrust{db: db, mac: mac}
}

// Token is the cookie value for a browser that has just signed in as username.
func (d *DeviceTrust) Token(ctx context.Context, username string) (string, error) {
	rec, err := d.db.UserForLogin(ctx, username)
	if err != nil {
		return "", fmt.Errorf("look up user: %w", err)
	}
	if rec == nil {
		return "", fmt.Errorf("look up user %q: not found", username)
	}
	sum, err := d.sign(rec)
	if err != nil {
		return "", err
	}
	return rec.ID + "." + sum, nil
}

// Trusted reports whether value is a current token for username. Any lookup failure is
// untrusted.
func (d *DeviceTrust) Trusted(ctx context.Context, username, value string) bool {
	id, sum, ok := strings.Cut(value, ".")
	if !ok || sum == "" {
		return false
	}
	rec, err := d.db.UserForLogin(ctx, username)
	if err != nil || rec == nil || rec.ID != id {
		return false
	}
	want, err := d.sign(rec)
	return err == nil && hmac.Equal([]byte(sum), []byte(want))
}

func (d *DeviceTrust) sign(rec *store.AuthRecord) (string, error) {
	sum, err := d.mac([]byte("trusted-device\x00" + rec.ID + "\x00" + rec.PasswordHash))
	if err != nil {
		return "", fmt.Errorf("sign trusted device: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}
