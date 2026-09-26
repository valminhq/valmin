package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// fastParams keeps the test suite from paying real argon2id cost — Decision 4's 64 MiB is
// a production number, not a test one.
var fastParams = Argon2Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want the PHC argon2id prefix", hash)
	}
	if !VerifyPassword("correct horse battery staple", hash) {
		t.Error("correct password did not verify")
	}
	if VerifyPassword("wrong password", hash) {
		t.Error("wrong password verified")
	}
}

func TestHashPasswordSaltsEveryCall(t *testing.T) {
	a, err := HashPassword("same password", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same password", fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not being randomised")
	}
	if !VerifyPassword("same password", a) || !VerifyPassword("same password", b) {
		t.Error("one of the two independently-salted hashes did not verify")
	}
}

func TestVerifyPasswordRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$argon2id$v=19$m=x,t=x,p=x$AA$AA", "$bcrypt$…"} {
		if VerifyPassword("anything", bad) {
			t.Errorf("VerifyPassword accepted malformed hash %q", bad)
		}
	}
}

// TestNeedsRehash is 10 §3.4: an invite issued (or a password hashed) before a parameter
// bump must be flagged so the caller rehashes on next successful verification.
func TestNeedsRehash(t *testing.T) {
	weak := Argon2Params{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	strong := Argon2Params{MemoryKiB: 16 * 1024, Time: 2, Threads: 2, SaltLen: 16, KeyLen: 32}

	hash, err := HashPassword("p", weak)
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(hash, weak) {
		t.Error("a hash at the current parameters was flagged for rehash")
	}
	if !NeedsRehash(hash, strong) {
		t.Error("a hash below current parameters was not flagged for rehash")
	}
	if !NeedsRehash("garbage", strong) {
		t.Error("an unparseable hash was not flagged for rehash")
	}
}

// TestDummyHashCostsARealVerification asserts what VerifyAgainstDummy relies on: the dummy
// hash parses as argon2id at the default cost. A malformed one would make VerifyPassword
// return at parseHash, so an unknown username would answer faster than a real one (11 §7).
func TestDummyHashCostsARealVerification(t *testing.T) {
	version, p, _, _, ok := parseHash(dummyHash)
	if !ok || version != argon2.Version {
		t.Fatalf("dummyHash %q does not parse as argon2id v%d", dummyHash, argon2.Version)
	}
	if p.MemoryKiB != DefaultArgon2Params.MemoryKiB || p.Time != DefaultArgon2Params.Time ||
		p.Threads != DefaultArgon2Params.Threads {
		t.Errorf("dummyHash params = %+v, want the defaults %+v", p, DefaultArgon2Params)
	}
	if VerifyPassword("anything at all", dummyHash) {
		t.Error("a guessed password verified against the dummy hash")
	}
}

func TestLoadArgon2ParamsWritesDefaultOnFirstUse(t *testing.T) {
	kv := &fakeKV{}
	p, err := LoadArgon2Params(t.Context(), kv)
	if err != nil {
		t.Fatal(err)
	}
	if p != DefaultArgon2Params {
		t.Errorf("first load = %+v, want the default %+v", p, DefaultArgon2Params)
	}
	if _, ok := kv.values[argon2ParamsKey]; !ok {
		t.Error("default parameters were not written back to kv, so there is nothing to raise later")
	}

	// A second load reads back what was written, not the default recomputed.
	raised := Argon2Params{MemoryKiB: 128 * 1024, Time: 4, Threads: 4, SaltLen: 16, KeyLen: 32}
	if err := kv.KVSet(t.Context(), argon2ParamsKey, raised); err != nil {
		t.Fatal(err)
	}
	got, err := LoadArgon2Params(t.Context(), kv)
	if err != nil {
		t.Fatal(err)
	}
	if got != raised {
		t.Errorf("second load = %+v, want the raised value %+v", got, raised)
	}
}
