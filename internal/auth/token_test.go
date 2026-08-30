package auth

import "testing"

func TestNewSessionTokenRoundTrips(t *testing.T) {
	cookieValue, tokenHash := NewSessionToken()
	if cookieValue == "" || tokenHash == "" {
		t.Fatalf("empty token: cookie=%q hash=%q", cookieValue, tokenHash)
	}
	got, err := HashSessionToken(cookieValue)
	if err != nil {
		t.Fatal(err)
	}
	if got != tokenHash {
		t.Errorf("re-derived hash %q, want %q", got, tokenHash)
	}
}

func TestNewSessionTokenIsUnique(t *testing.T) {
	a, aHash := NewSessionToken()
	b, bHash := NewSessionToken()
	if a == b || aHash == bHash {
		t.Fatal("two calls produced the same token")
	}
}

func TestHashSessionTokenRejectsGarbage(t *testing.T) {
	if _, err := HashSessionToken("not valid base64url!!"); err == nil {
		t.Error("a malformed cookie value was accepted")
	}
	if _, ok := tryHashSessionToken("not valid base64url!!"); ok {
		t.Error("tryHashSessionToken reported ok on malformed input")
	}
}

func TestNewSetupTokenIsSessionShaped(t *testing.T) {
	token, hash := NewSetupToken()
	got, err := HashSessionToken(token)
	if err != nil || got != hash {
		t.Errorf("setup token does not round-trip like a session token: got=%q err=%v", got, err)
	}
}

func TestNewInviteTokenIsHumanReadableAndVerifies(t *testing.T) {
	code, hash, err := NewInviteToken(fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if code == "" || hash == "" {
		t.Fatalf("empty invite token: code=%q hash=%q", code, hash)
	}
	// Base32 unpadded, per 09 §5 — no '=' padding and no lowercase.
	for _, r := range code {
		if r == '=' {
			t.Errorf("code %q is padded, want unpadded base32", code)
		}
	}
	if !VerifyPassword(code, hash) {
		t.Error("the issued code does not verify against its own hash")
	}
}

func TestNewInviteTokenIsUnique(t *testing.T) {
	a, _, err := NewInviteToken(fastParams)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := NewInviteToken(fastParams)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two invite tokens are identical")
	}
}
