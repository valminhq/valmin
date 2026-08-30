package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// generateToken returns n bytes of CSPRNG. crypto/rand failing is unreachable on Linux and
// has nothing sane to fall back to (the same judgment lease.go's randomID makes).
func generateToken(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return b
}

// hashToken returns the hex SHA-256 of raw. Sessions and the setup token are both a bare
// CSPRNG value looked up on every use — a fast hash is correct here and a slow one is a
// self-inflicted latency bug (10 §4.1). This is the same reasoning applied to both, unlike
// invites, which 09 §5 explicitly hashes like a password because redemption is rare enough
// that the slower comparison costs nothing real.
func hashToken(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// NewSessionToken returns a 32-byte CSPRNG cookie value and the hash the DB stores of it
// (10 §4.1).
func NewSessionToken() (cookieValue, tokenHash string) {
	raw := generateToken(32)
	return base64.RawURLEncoding.EncodeToString(raw), hashToken(raw)
}

// HashSessionToken re-derives a session's hash from its cookie value, for lookup.
func HashSessionToken(cookieValue string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cookieValue)
	if err != nil {
		return "", fmt.Errorf("decode session token: %w", err)
	}
	return hashToken(raw), nil
}

// tryHashSessionToken is HashSessionToken for a caller to whom a malformed cookie is not
// an error to propagate — a tampered or garbage cookie is simply "not authenticated" — so
// the outcome is a bool rather than an error a linter would read as one being swallowed.
func tryHashSessionToken(cookieValue string) (hash string, ok bool) {
	hash, err := HashSessionToken(cookieValue)
	return hash, err == nil
}

// NewSetupToken returns a 32-byte CSPRNG token and its hash for kv["bootstrap_state"]
// (10 §6). Same reasoning as a session token: compared rarely, but it is a bare CSPRNG
// value rather than a password, so SHA-256 is the correct hash, not argon2id.
func NewSetupToken() (token, tokenHash string) { return NewSessionToken() }

// RandomPassword generates a password for the two flows that hand one to an operator
// rather than take one: admin-issued reset and the CLI recovery command (09 §5, 09 §6). It
// is shown once and never stored in this form.
func RandomPassword() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(generateToken(15))
}

// NewInviteToken returns a 32-byte CSPRNG code and its argon2id hash for storage (09 §5).
// Base32 unpadded so a human can read it down a phone line, per the same section.
func NewInviteToken(params Argon2Params) (code, hash string, err error) {
	raw := generateToken(32)
	code = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	hash, err = HashPassword(code, params)
	if err != nil {
		return "", "", fmt.Errorf("hash invite token: %w", err)
	}
	return code, hash, nil
}
