// Package auth implements first-run bootstrap, login, sessions and invites.
//
// Specification: 10 §3.4, 10 §4.1, 10 §6, 09 §5.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2ParamsKey is the kv row Decision 4 puts the parameters in, so they can be raised
// without a code change (10 §3.4).
const argon2ParamsKey = "argon2_params"

// Argon2Params is Decision 4's own number: m=64MiB t=3 p=2 saltLen=16 keyLen=32.
type Argon2Params struct {
	MemoryKiB uint32 `json:"memory_kib"`
	Time      uint32 `json:"time"`
	Threads   uint8  `json:"threads"`
	SaltLen   uint32 `json:"salt_len"`
	KeyLen    uint32 `json:"key_len"`
}

// DefaultArgon2Params is Decision 4's number, not a measurement — it is `10 §3.4`'s own
// figure, decidable by policy (PLANNING-BRIEF §7).
var DefaultArgon2Params = Argon2Params{MemoryKiB: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// KV is the subset of the store this package needs, defined by the consumer (06 §4).
type KV interface {
	KVGet(ctx context.Context, key string, v any) (bool, error)
	KVSet(ctx context.Context, key string, v any) error
}

// LoadArgon2Params reads the current parameters, writing the default on first use — the
// same pattern the crypto package uses for active_key_id, so the row is visible and
// editable from day one rather than an implicit constant.
func LoadArgon2Params(ctx context.Context, kv KV) (Argon2Params, error) {
	var p Argon2Params
	found, err := kv.KVGet(ctx, argon2ParamsKey, &p)
	if err != nil {
		return Argon2Params{}, fmt.Errorf("load argon2 parameters: %w", err)
	}
	if found {
		return p, nil
	}
	if err := kv.KVSet(ctx, argon2ParamsKey, DefaultArgon2Params); err != nil {
		return Argon2Params{}, fmt.Errorf("store default argon2 parameters: %w", err)
	}
	return DefaultArgon2Params, nil
}

// HashPassword renders the PHC-style string this package stores and compares:
// $argon2id$v=19$m=<kib>,t=<time>,p=<threads>$<salt>$<hash>, both fields base64 raw.
func HashPassword(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword reports whether password matches encoded, in constant time. A malformed
// encoded value is a mismatch, never an error a caller could use to distinguish "wrong
// password" from "corrupt row".
func VerifyPassword(password, encoded string) bool {
	version, p, salt, sum, ok := parseHash(encoded)
	if !ok || version != argon2.Version {
		return false
	}
	// A stored hash's output length is bounded by what base64 could have decoded from a
	// database column, nowhere near uint32's range — but the conversion below is exact
	// only with the guard, so it is asserted rather than assumed.
	keyLen := len(sum)
	if keyLen > math.MaxUint32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(keyLen))
	return subtle.ConstantTimeCompare(got, sum) == 1
}

// NeedsRehash reports whether encoded was hashed under weaker parameters than current —
// 10 §3.4: on successful login, if the stored hash's parameters are below current
// settings, rehash and store.
func NeedsRehash(encoded string, current Argon2Params) bool {
	_, p, _, _, ok := parseHash(encoded)
	if !ok {
		return true
	}
	return p.MemoryKiB < current.MemoryKiB || p.Time < current.Time || p.Threads < current.Threads
}

func parseHash(encoded string) (version int, p Argon2Params, salt, sum []byte, ok bool) {
	// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, Argon2Params{}, nil, nil, false
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, Argon2Params{}, nil, nil, false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return 0, Argon2Params{}, nil, nil, false
	}
	var err error
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return 0, Argon2Params{}, nil, nil, false
	}
	if sum, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return 0, Argon2Params{}, nil, nil, false
	}
	return version, p, salt, sum, true
}

// dummyHash is compared against on a login for a username that does not exist, so the two
// cases cost the same work and the timing difference cannot enumerate users (11 §7).
// Generated once, at whatever parameters were current then — its own freshness does not
// matter, since nothing ever authenticates against it for real.
var dummyHash = func() string {
	h, err := HashPassword("valmin-dummy-password-for-timing-parity", DefaultArgon2Params)
	if err != nil {
		// Only crypto/rand failing could cause this, which is unreachable on Linux and
		// has nothing sane to fall back to — the same judgment call store.randomID makes.
		panic("auth: hashing the dummy password: " + err.Error())
	}
	return h
}()

// VerifyAgainstDummy always fails, but takes the same time as a real verification.
func VerifyAgainstDummy(password string) { VerifyPassword(password, dummyHash) }
