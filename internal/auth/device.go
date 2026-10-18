package auth

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// DeviceTrustAge is how long a browser stays trusted after it signed in.
const DeviceTrustAge = 180 * 24 * time.Hour

// DeviceTrust signs and checks the value of the cookie that marks a browser where an account
// has signed in. The signature covers the account's id, the time it was issued and the current
// password hash, so a token expires after DeviceTrustAge and changing or resetting the password
// withdraws every browser's trust at once.
type DeviceTrust struct {
	db  *store.DB
	mac func(msg []byte) ([]byte, error)
	now func() time.Time
}

// NewDeviceTrust signs with mac, which must be a keyed MAC the panel holds alone.
func NewDeviceTrust(db *store.DB, mac func(msg []byte) ([]byte, error)) *DeviceTrust {
	return &DeviceTrust{db: db, mac: mac, now: time.Now}
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
	issued := strconv.FormatInt(d.now().Unix(), 10)
	sum, err := d.sign(rec, issued)
	if err != nil {
		return "", err
	}
	return rec.ID + "." + issued + "." + sum, nil
}

// Trusted reports whether value is an unexpired token for username's current password. Any
// lookup failure is untrusted.
func (d *DeviceTrust) Trusted(ctx context.Context, username, value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[2] == "" {
		return false
	}
	id, issued, sum := parts[0], parts[1], parts[2]
	at, err := strconv.ParseInt(issued, 10, 64)
	if err != nil {
		return false
	}
	if age := d.now().Sub(time.Unix(at, 0)); age < 0 || age > DeviceTrustAge {
		return false
	}
	rec, err := d.db.UserForLogin(ctx, username)
	if err != nil || rec == nil || rec.ID != id {
		return false
	}
	want, err := d.sign(rec, issued)
	return err == nil && hmac.Equal([]byte(sum), []byte(want))
}

func (d *DeviceTrust) sign(rec *store.AuthRecord, issued string) (string, error) {
	sum, err := d.mac([]byte("trusted-device\x00" + rec.ID + "\x00" + issued + "\x00" + rec.PasswordHash))
	if err != nil {
		return "", fmt.Errorf("sign trusted device: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}
