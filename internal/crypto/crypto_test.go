package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// noEnv is the environment of a panel that lets the daemon own its key file.
func noEnv(string) string { return "" }

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// keeper returns a Keeper over fixed key material, so a test can build a second one for
// the same generation or a different one.
func keeper(t *testing.T, activeKeyID string) *Keeper {
	t.Helper()
	k, err := NewKeeper(bytes.Repeat([]byte{7}, MasterKeyLen), []byte("salt"), activeKeyID)
	if err != nil {
		t.Fatalf("NewKeeper: %v", err)
	}
	return k
}

// kvStore returns a migrated database, which satisfies KV.
func kvStore(t *testing.T) *store.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "panel.db")
	db, err := store.Open(t.Context(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

var (
	rowA = Location{Table: "instances", Column: "password", RowID: "row-a"}
	rowB = Location{Table: "instances", Column: "password", RowID: "row-b"}
)

func TestRoundTrip(t *testing.T) {
	k := keeper(t, firstKeyID)
	envelope, err := k.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := k.Decrypt(PurposeInstancePassword, rowA, envelope)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != "valheim" {
		t.Errorf("Decrypt = %q, want %q", got, "valheim")
	}
	if strings.Contains(envelope, "valheim") {
		t.Errorf("envelope %q contains the plaintext", envelope)
	}
}

// TestAADBindsToItsLocation covers D5. Panel access must not let a value be shuffled
// between rows or columns, so the tag has to fail rather than merely mismatch.
func TestAADBindsToItsLocation(t *testing.T) {
	k := keeper(t, firstKeyID)
	envelope, err := k.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tests := map[string]struct {
		purpose Purpose
		loc     Location
	}{
		"another row":    {PurposeInstancePassword, rowB},
		"another column": {PurposeInstancePassword, Location{"instances", "rcon_password", "row-a"}},
		"another table":  {PurposeInstancePassword, Location{"invites", "password", "row-a"}},
		"another purpose": {
			PurposeRCONPassword,
			Location{"instances", "password", "row-a"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := k.Decrypt(tc.purpose, tc.loc, envelope)
			if !errors.Is(err, ErrDecrypt) {
				t.Fatalf("Decrypt from %s = %v, want ErrDecrypt", name, err)
			}
		})
	}
}

// TestAADIsUnambiguous guards the length prefixing: a plain concatenation would let a
// table and column shift a character between them and still authenticate.
func TestAADIsUnambiguous(t *testing.T) {
	k := keeper(t, firstKeyID)
	envelope, err := k.Encrypt(PurposeInstancePassword, Location{"ab", "c", "r"}, []byte("x"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(PurposeInstancePassword, Location{"a", "bc", "r"}, envelope); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt with a shifted field boundary = %v, want ErrDecrypt", err)
	}
}

func TestTamperedEnvelopeFails(t *testing.T) {
	k := keeper(t, firstKeyID)
	envelope, err := k.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	parts := strings.Split(envelope, ".")
	sealed, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	sealed[0] ^= 0xff
	parts[3] = base64.RawURLEncoding.EncodeToString(sealed)

	if _, err := k.Decrypt(PurposeInstancePassword, rowA, strings.Join(parts, ".")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt of a flipped byte = %v, want ErrDecrypt", err)
	}
}

// TestOldGenerationStillDecrypts covers 10 §3.3: rotation is a re-encrypt job, so a value
// written under the previous key id has to keep opening while that job runs.
func TestOldGenerationStillDecrypts(t *testing.T) {
	old := keeper(t, firstKeyID)
	envelope, err := old.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	rotated := keeper(t, "2")
	got, err := rotated.Decrypt(PurposeInstancePassword, rowA, envelope)
	if err != nil {
		t.Fatalf("Decrypt under a flipped active_key_id: %v", err)
	}
	if string(got) != "valheim" {
		t.Errorf("Decrypt = %q, want %q", got, "valheim")
	}

	fresh, err := rotated.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(fresh, "v1.2.") {
		t.Errorf("new ciphertext is %q, want the active generation in the envelope", fresh)
	}
}

// TestGenerationsAreIndependent proves key_id is real key separation rather than a label.
func TestGenerationsAreIndependent(t *testing.T) {
	envelope, err := keeper(t, firstKeyID).Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	relabelled := strings.Replace(envelope, "v1."+firstKeyID+".", "v1.2.", 1)
	if _, err := keeper(t, "2").Decrypt(PurposeInstancePassword, rowA, relabelled); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Decrypt of a relabelled envelope = %v, want ErrDecrypt", err)
	}
}

func TestEnvelopeShape(t *testing.T) {
	envelope, err := keeper(t, firstKeyID).Encrypt(PurposeInstancePassword, rowA, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	parts := strings.Split(envelope, ".")
	if len(parts) != envelopeParts {
		t.Fatalf("envelope %q has %d parts, want %d", envelope, len(parts), envelopeParts)
	}
	if parts[0] != "v1" || parts[1] != firstKeyID {
		t.Errorf("envelope prefix is %q.%q, want %q.%q", parts[0], parts[1], "v1", firstKeyID)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	if len(nonce) != 24 {
		t.Errorf("nonce is %d bytes, want 24 for XChaCha20-Poly1305", len(nonce))
	}
}

func TestNoncesDoNotRepeat(t *testing.T) {
	k := keeper(t, firstKeyID)
	seen := map[string]bool{}
	for range 1000 {
		envelope, err := k.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		nonce := strings.Split(envelope, ".")[2]
		if seen[nonce] {
			t.Fatalf("nonce %s reused", nonce)
		}
		seen[nonce] = true
	}
}

func TestMalformedEnvelopes(t *testing.T) {
	k := keeper(t, firstKeyID)
	envelope, err := k.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	parts := strings.Split(envelope, ".")

	tests := map[string]string{
		"empty":            "",
		"too few parts":    "v1." + parts[1] + "." + parts[2],
		"too many parts":   envelope + "." + parts[3],
		"wrong version":    "v2." + parts[1] + "." + parts[2] + "." + parts[3],
		"empty key id":     "v1.." + parts[2] + "." + parts[3],
		"space in key id":  "v1.a b." + parts[2] + "." + parts[3],
		"bad nonce b64":    "v1." + parts[1] + ".!!!." + parts[3],
		"short nonce":      "v1." + parts[1] + ".AAAA." + parts[3],
		"bad ciphertext":   "v1." + parts[1] + "." + parts[2] + ".!!!",
		"empty ciphertext": "v1." + parts[1] + "." + parts[2] + ".",
	}
	for name, envelope := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := k.Decrypt(PurposeInstancePassword, rowA, envelope); err == nil {
				t.Fatalf("Decrypt of a %s envelope succeeded", name)
			}
		})
	}
}

// TestUnknownPurposeIsRejected keeps a typo from silently deriving a different key.
func TestUnknownPurposeIsRejected(t *testing.T) {
	k := keeper(t, firstKeyID)
	if _, err := k.Encrypt(Purpose("instance_password"), rowA, []byte("x")); err == nil {
		t.Fatal("Encrypt with an unknown purpose succeeded")
	}
}

// TestPurposesAreTheSpecifiedFive covers 10 §3.1's rule that key loss costs passwords and
// TOTP secrets, never a world. A purpose that encrypts anything under worlds/ would break
// it, so the set is closed here rather than in review prose.
func TestPurposesAreTheSpecifiedFive(t *testing.T) {
	want := []Purpose{
		PurposeInstancePassword, PurposeRCONPassword, PurposeTOTPSecret,
		PurposeCookieMAC, PurposeCSRF,
	}
	if len(purposes) != len(want) {
		t.Fatalf("there are %d purposes, want the %d of 10 §3.2", len(purposes), len(want))
	}
	for _, p := range want {
		if !purposes[p] {
			t.Errorf("purpose %q is missing", p)
		}
	}
}

func TestMACIsPurposeSeparated(t *testing.T) {
	k := keeper(t, firstKeyID)
	csrf, err := k.MAC(PurposeCSRF, []byte("session-1"))
	if err != nil {
		t.Fatalf("MAC: %v", err)
	}
	cookie, err := k.MAC(PurposeCookieMAC, []byte("session-1"))
	if err != nil {
		t.Fatalf("MAC: %v", err)
	}
	if bytes.Equal(csrf, cookie) {
		t.Error("the csrf and cookie-mac subkeys produce the same MAC")
	}

	again, err := k.MAC(PurposeCSRF, []byte("session-1"))
	if err != nil {
		t.Fatalf("MAC: %v", err)
	}
	if !bytes.Equal(csrf, again) {
		t.Error("MAC is not deterministic")
	}
}

func TestNewKeeperRejectsBadMaterial(t *testing.T) {
	full := bytes.Repeat([]byte{7}, MasterKeyLen)
	tests := map[string]struct {
		key   []byte
		salt  []byte
		keyID string
	}{
		"short master key": {full[:16], []byte("salt"), firstKeyID},
		"empty salt":       {full, nil, firstKeyID},
		"key id with dot":  {full, []byte("salt"), "1.2"},
		"empty key id":     {full, []byte("salt"), ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewKeeper(tc.key, tc.salt, tc.keyID); err == nil {
				t.Fatalf("NewKeeper with a %s succeeded", name)
			}
		})
	}
}

// TestMasterKeyFileIsCreatedOnFirstStart covers 10 §3.1's mode and size, and the rule
// that the key is never chowned after creation (Q14).
func TestMasterKeyFileIsCreatedOnFirstStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")

	key, err := LoadMasterKey(path, noEnv)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != MasterKeyLen {
		t.Fatalf("generated key is %d bytes, want %d", len(key), MasterKeyLen)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != keyFileMode {
		t.Errorf("generated key has mode %#o, want %#o", perm, keyFileMode)
	}

	again, err := LoadMasterKey(path, noEnv)
	if err != nil {
		t.Fatalf("LoadMasterKey on the second start: %v", err)
	}
	if !bytes.Equal(key, again) {
		t.Error("the second start read a different key")
	}
}

func TestMasterKeySources(t *testing.T) {
	full := bytes.Repeat([]byte{7}, MasterKeyLen)
	b64 := base64.StdEncoding.EncodeToString(full)

	t.Run("inline env", func(t *testing.T) {
		key, err := LoadMasterKey("", env(map[string]string{EnvMasterKey: b64}))
		if err != nil {
			t.Fatalf("LoadMasterKey: %v", err)
		}
		if !bytes.Equal(key, full) {
			t.Error("LoadMasterKey returned different bytes")
		}
	})

	t.Run("inline env with a trailing newline", func(t *testing.T) {
		if _, err := LoadMasterKey("", env(map[string]string{EnvMasterKey: b64 + "\n"})); err != nil {
			t.Fatalf("LoadMasterKey: %v", err)
		}
	})

	t.Run("file env", func(t *testing.T) {
		// Mounted secrets are commonly world-readable; the operator-managed path is not
		// mode-checked, unlike the panel-managed one.
		path := filepath.Join(t.TempDir(), "master")
		if err := os.WriteFile(path, []byte(b64), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		key, err := LoadMasterKey("", env(map[string]string{EnvMasterKeyFile: path}))
		if err != nil {
			t.Fatalf("LoadMasterKey: %v", err)
		}
		if !bytes.Equal(key, full) {
			t.Error("LoadMasterKey returned different bytes")
		}
	})

	t.Run("both is an error", func(t *testing.T) {
		_, err := LoadMasterKey("", env(map[string]string{
			EnvMasterKey:     b64,
			EnvMasterKeyFile: "/tmp/master",
		}))
		if err == nil {
			t.Fatal("both sources set was accepted")
		}
		if !strings.Contains(err.Error(), EnvMasterKey) || !strings.Contains(err.Error(), EnvMasterKeyFile) {
			t.Errorf("error %q names neither variable", err)
		}
	})

	t.Run("env value stays out of the error", func(t *testing.T) {
		_, err := LoadMasterKey("", env(map[string]string{EnvMasterKey: "not base64!"}))
		if err == nil {
			t.Fatal("a non-base64 key was accepted")
		}
		if strings.Contains(err.Error(), "not base64!") {
			t.Errorf("error %q leaks the supplied value", err)
		}
	})

	t.Run("wrong length", func(t *testing.T) {
		short := base64.StdEncoding.EncodeToString(full[:16])
		if _, err := LoadMasterKey("", env(map[string]string{EnvMasterKey: short})); err == nil {
			t.Fatal("a 16-byte key was accepted")
		}
	})
}

func TestMasterKeyFileIsRejected(t *testing.T) {
	full := bytes.Repeat([]byte{7}, MasterKeyLen)

	tests := map[string]struct {
		contents []byte
		mode     os.FileMode
	}{
		"world readable": {full, 0o644},
		"group readable": {full, 0o640},
		"short":          {full[:16], keyFileMode},
		"base64 text":    {[]byte(base64.StdEncoding.EncodeToString(full)), keyFileMode},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.key")
			if err := os.WriteFile(path, tc.contents, tc.mode); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if _, err := LoadMasterKey(path, noEnv); err == nil {
				t.Fatalf("a %s key file was accepted", name)
			}
		})
	}
}

func TestMasterKeyDirectoryIsRejected(t *testing.T) {
	if _, err := LoadMasterKey(t.TempDir(), noEnv); err == nil {
		t.Fatal("a directory was accepted as a master key")
	}
}

// TestOpenPersistsSaltAndGeneration is the regression guard that matters most: a salt
// that is not stable across restarts makes every stored secret unreadable.
func TestOpenPersistsSaltAndGeneration(t *testing.T) {
	db := kvStore(t)
	path := filepath.Join(t.TempDir(), "secret.key")

	first, err := Open(t.Context(), db, path, noEnv)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if first.ActiveKeyID() != firstKeyID {
		t.Errorf("active key id is %q, want %q", first.ActiveKeyID(), firstKeyID)
	}
	envelope, err := first.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	second, err := Open(t.Context(), db, path, noEnv)
	if err != nil {
		t.Fatalf("Open on restart: %v", err)
	}
	got, err := second.Decrypt(PurposeInstancePassword, rowA, envelope)
	if err != nil {
		t.Fatalf("Decrypt after restart: %v", err)
	}
	if string(got) != "valheim" {
		t.Errorf("Decrypt = %q, want %q", got, "valheim")
	}
}

// TestOpenHonoursARotatedActiveKeyID proves the write generation comes from kv, so M6's
// rotate endpoint is a kv flip rather than a code change.
func TestOpenHonoursARotatedActiveKeyID(t *testing.T) {
	db := kvStore(t)
	path := filepath.Join(t.TempDir(), "secret.key")

	first, err := Open(t.Context(), db, path, noEnv)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	envelope, err := first.Encrypt(PurposeInstancePassword, rowA, []byte("valheim"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if err := db.KVSet(t.Context(), activeKeyIDKey, "2"); err != nil {
		t.Fatalf("KVSet: %v", err)
	}

	rotated, err := Open(t.Context(), db, path, noEnv)
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	if rotated.ActiveKeyID() != "2" {
		t.Fatalf("active key id is %q, want %q", rotated.ActiveKeyID(), "2")
	}
	if _, err := rotated.Decrypt(PurposeInstancePassword, rowA, envelope); err != nil {
		t.Fatalf("Decrypt of a previous generation: %v", err)
	}
}
