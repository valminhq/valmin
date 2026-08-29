package crypto

import (
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// Purpose names a subkey. The master key is never used directly: each purpose gets an
// independent HKDF-SHA256 subkey (10 §3.2).
type Purpose string

const (
	PurposeInstancePassword Purpose = "instance-password"
	PurposeRCONPassword     Purpose = "rcon-password"
	PurposeTOTPSecret       Purpose = "totp-secret"
	PurposeCookieMAC        Purpose = "cookie-mac"
	PurposeCSRF             Purpose = "csrf"
)

var purposes = map[Purpose]bool{
	PurposeInstancePassword: true,
	PurposeRCONPassword:     true,
	PurposeTOTPSecret:       true,
	PurposeCookieMAC:        true,
	PurposeCSRF:             true,
}

// Location is where a ciphertext lives. It is bound into the AAD, so a value cannot be
// lifted between rows or columns without failing the tag (10 §3.2).
type Location struct {
	Table  string
	Column string
	RowID  string
}

// MasterKeyLen is the size of the master key on disk (10 §3.1).
const MasterKeyLen = 32

const (
	envelopeVersion = "v1"
	envelopeParts   = 4
	saltLen         = 32
	subkeyLen       = 32

	// keySaltKey and activeKeyIDKey are reserved kv keys (10 §4.2).
	keySaltKey     = "key_salt"
	activeKeyIDKey = "active_key_id"

	// firstKeyID is the generation a fresh panel writes with. Rotation mints the next
	// one; the endpoint that does so is M6 (10 §3.3).
	firstKeyID = "1"
)

// keyIDPattern keeps a key id out of the envelope's separator and out of anything a log
// line has to escape.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrDecrypt reports that a ciphertext did not authenticate. It never distinguishes a
// tampered tag from a wrong location: both mean the value is not usable here.
var ErrDecrypt = errors.New("decrypt: ciphertext failed authentication")

// KV is the subset of the panel's key-value table this package needs.
type KV interface {
	KVGet(ctx context.Context, key string, v any) (bool, error)
	KVSet(ctx context.Context, key string, v any) error
}

// Keeper derives subkeys and seals values into the envelope of 10 §3.2.
type Keeper struct {
	masterKey   []byte
	salt        []byte
	activeKeyID string
}

// Open loads the master key, settles the HKDF salt and the active key generation, and
// returns a Keeper ready to seal. Salt and generation are created on first start.
//
// Losing the salt costs exactly what losing the master key costs (10 §3.1): instance
// passwords, RCON passwords and TOTP secrets. Never a world.
func Open(ctx context.Context, kv KV, masterKeyPath string, getenv func(string) string) (*Keeper, error) {
	masterKey, err := LoadMasterKey(masterKeyPath, getenv)
	if err != nil {
		return nil, err
	}

	var saltB64 string
	found, err := kv.KVGet(ctx, keySaltKey, &saltB64)
	if err != nil {
		return nil, fmt.Errorf("load hkdf salt: %w", err)
	}
	var salt []byte
	if found {
		if salt, err = base64.StdEncoding.DecodeString(saltB64); err != nil {
			return nil, fmt.Errorf("decode kv %q: %w", keySaltKey, err)
		}
	} else {
		salt = make([]byte, saltLen)
		if _, err := rand.Read(salt); err != nil {
			return nil, fmt.Errorf("generate hkdf salt: %w", err)
		}
		if err := kv.KVSet(ctx, keySaltKey, base64.StdEncoding.EncodeToString(salt)); err != nil {
			return nil, fmt.Errorf("store hkdf salt: %w", err)
		}
		slog.InfoContext(ctx, "generated hkdf salt",
			slog.String("kv_key", keySaltKey),
			slog.String("note", "back up the database and secret.key together"))
	}

	activeKeyID := firstKeyID
	found, err = kv.KVGet(ctx, activeKeyIDKey, &activeKeyID)
	if err != nil {
		return nil, fmt.Errorf("load active key id: %w", err)
	}
	if !found {
		if err := kv.KVSet(ctx, activeKeyIDKey, activeKeyID); err != nil {
			return nil, fmt.Errorf("store active key id: %w", err)
		}
	}

	return NewKeeper(masterKey, salt, activeKeyID)
}

// NewKeeper builds a Keeper from key material already in hand.
func NewKeeper(masterKey, salt []byte, activeKeyID string) (*Keeper, error) {
	if len(masterKey) != MasterKeyLen {
		return nil, fmt.Errorf("master key is %d bytes, want %d", len(masterKey), MasterKeyLen)
	}
	if len(salt) == 0 {
		return nil, errors.New("hkdf salt is empty")
	}
	if !keyIDPattern.MatchString(activeKeyID) {
		return nil, fmt.Errorf("key id %q: want 1-64 characters of [A-Za-z0-9_-]", activeKeyID)
	}
	return &Keeper{masterKey: masterKey, salt: salt, activeKeyID: activeKeyID}, nil
}

// ActiveKeyID names the generation new ciphertexts are sealed under.
func (k *Keeper) ActiveKeyID() string { return k.activeKeyID }

// Encrypt seals plaintext under the active generation and returns the envelope
// v1.<key_id>.<nonce>.<ciphertext+tag>.
func (k *Keeper) Encrypt(p Purpose, loc Location, plaintext []byte) (string, error) {
	aead, err := k.aead(p, k.activeKeyID)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, aad(p, loc))
	return strings.Join([]string{
		envelopeVersion,
		k.activeKeyID,
		base64.RawURLEncoding.EncodeToString(nonce),
		base64.RawURLEncoding.EncodeToString(sealed),
	}, "."), nil
}

// Decrypt opens an envelope written under any generation, which is what makes rotation a
// re-encrypt job rather than a migration (10 §3.3).
func (k *Keeper) Decrypt(p Purpose, loc Location, envelope string) ([]byte, error) {
	parts := strings.Split(envelope, ".")
	if len(parts) != envelopeParts {
		return nil, fmt.Errorf("envelope has %d parts, want %d", len(parts), envelopeParts)
	}
	if parts[0] != envelopeVersion {
		return nil, fmt.Errorf("envelope version %q is not %s", parts[0], envelopeVersion)
	}
	if !keyIDPattern.MatchString(parts[1]) {
		return nil, fmt.Errorf("envelope key id %q: want 1-64 characters of [A-Za-z0-9_-]", parts[1])
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode envelope nonce: %w", err)
	}
	if len(nonce) != chacha20poly1305.NonceSizeX {
		return nil, fmt.Errorf("envelope nonce is %d bytes, want %d", len(nonce), chacha20poly1305.NonceSizeX)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("decode envelope ciphertext: %w", err)
	}

	aead, err := k.aead(p, parts[1])
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, sealed, aad(p, loc))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// MAC returns HMAC-SHA256 of msg under the purpose's subkey. Subkeys are not exported:
// the CSRF token and the cookie MAC (10 §3.2, 11 §6.2) are the only callers, and neither
// needs the key material itself.
func (k *Keeper) MAC(p Purpose, msg []byte) ([]byte, error) {
	sub, err := k.subkey(p, k.activeKeyID)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, sub)
	mac.Write(msg)
	return mac.Sum(nil), nil
}

// subkey derives the per-purpose, per-generation key. The generation is part of the HKDF
// info, so every key id that was ever active stays derivable from the same master key and
// salt, and rotation adds no persistent state.
//
// Generations separate subkeys, not master keys: a new generation does not remediate a
// leaked master key (ADR-046, Q26).
func (k *Keeper) subkey(p Purpose, keyID string) ([]byte, error) {
	if !purposes[p] {
		return nil, fmt.Errorf("unknown purpose %q", p)
	}
	sub, err := hkdf.Key(sha256.New, k.masterKey, k.salt, keyID+"/"+string(p), subkeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive subkey for %q: %w", p, err)
	}
	return sub, nil
}

func (k *Keeper) aead(p Purpose, keyID string) (cipher.AEAD, error) {
	sub, err := k.subkey(p, keyID)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(sub)
	if err != nil {
		return nil, fmt.Errorf("new XChaCha20-Poly1305: %w", err)
	}
	return aead, nil
}

// aad builds purpose || table || column || row_id. Fields are length-prefixed rather than
// joined, so ("ab", "c") and ("a", "bc") cannot produce the same associated data.
func aad(p Purpose, loc Location) []byte {
	var b []byte
	for _, s := range []string{string(p), loc.Table, loc.Column, loc.RowID} {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}
