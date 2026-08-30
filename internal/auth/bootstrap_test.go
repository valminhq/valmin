package auth

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func TestBootstrapPendingReflectsUserCount(t *testing.T) {
	db := testDB(t)
	b := NewBootstrap(db)

	pending, err := b.Pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("a fresh panel with no users reports itself as not pending")
	}

	if err := db.CreateUser(t.Context(), "u1", "ada", "h", store.RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}
	if pending, err = b.Pending(t.Context()); err != nil {
		t.Fatal(err)
	} else if pending {
		t.Error("a panel with an admin still reports pending")
	}
}

// TestPrintTokenIsFramedAndOncePerCall is 10 §6: printed once per process start, and
// framed clearly enough that an operator scrolling a container log will not miss it.
func TestPrintTokenIsFramedAndOncePerCall(t *testing.T) {
	db := testDB(t)
	b := NewBootstrap(db)
	var buf bytes.Buffer

	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Valmin first-run setup") {
		t.Errorf("output does not name itself: %q", out)
	}

	// D10: the setup token is the one secret that legitimately appears in a log line —
	// it is meant to be read from stdout — but it must not appear anywhere else, and this
	// asserts the one place it does is framed, not buried in an unrelated line.
	token := extractToken(t, out)
	if token == "" {
		t.Fatalf("could not find a token in the printed output: %q", out)
	}
}

// TestPrintTokenIsSilentOncePending holds the other half of 10 §6: once an admin exists,
// nothing more is printed — a restarted panel with a real admin does not leak a live
// setup token into the log on every boot.
func TestPrintTokenIsSilentOncePending(t *testing.T) {
	db := testDB(t)
	b := NewBootstrap(db)
	if err := db.CreateUser(t.Context(), "u1", "ada", "h", store.RoleAdmin, time.Now()); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("PrintToken wrote %q for a panel that already has an admin", buf.String())
	}
}

// TestPrintTokenRegeneratesEveryCall is 10 §6: "regenerated on each restart while
// unconsumed" — the previous token must stop working once a new one is printed.
func TestPrintTokenRegeneratesEveryCall(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	b := NewBootstrap(db)

	var first, second bytes.Buffer
	if err := b.PrintToken(t.Context(), &first); err != nil {
		t.Fatal(err)
	}
	if err := b.PrintToken(t.Context(), &second); err != nil {
		t.Fatal(err)
	}
	oldToken := extractToken(t, first.String())
	newToken := extractToken(t, second.String())
	if oldToken == newToken {
		t.Fatal("two restarts printed the same token")
	}

	if _, err := b.Setup(t.Context(), oldToken, "ada", "a-fine-password"); !errors.Is(err, ErrSetupTokenInvalid) {
		t.Errorf("the superseded token was accepted: %v", err)
	}
	if _, err := b.Setup(t.Context(), newToken, "ada", "a-fine-password"); err != nil {
		t.Errorf("the current token was rejected: %v", err)
	}
}

func extractToken(t *testing.T, printed string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s{6}(\S+)\s*$`).FindStringSubmatch(printed)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestSetupCreatesTheFirstAdmin(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	b := NewBootstrap(db)

	var buf bytes.Buffer
	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, buf.String())

	u, err := b.Setup(t.Context(), token, "ada", "a-fine-password")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "ada" || u.Role != store.RoleAdmin {
		t.Errorf("created user = %+v, want admin ada", u)
	}

	rec, err := db.UserForLogin(t.Context(), "ada")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("a-fine-password", rec.PasswordHash) {
		t.Error("the created admin's password does not verify")
	}
}

// TestSetupIsConsumedForever is 10 §6: 410 forever once a user exists, no re-bootstrap
// path — checked across a second attempt with the same token and with a fresh one.
func TestSetupIsConsumedForever(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	b := NewBootstrap(db)

	var buf bytes.Buffer
	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, buf.String())
	if _, err := b.Setup(t.Context(), token, "ada", "a-fine-password"); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Setup(t.Context(), token, "bea", "another-password"); !errors.Is(err, ErrSetupConsumed) {
		t.Errorf("re-using the token after setup = %v, want ErrSetupConsumed", err)
	}

	var second bytes.Buffer
	if err := b.PrintToken(t.Context(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 {
		t.Error("a token was printed for an already-bootstrapped panel")
	}
}

func TestSetupRejectsAWrongToken(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	b := NewBootstrap(db)
	var buf bytes.Buffer
	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}

	_, err := b.Setup(t.Context(), "definitely-not-the-token", "ada", "a-fine-password")
	if !errors.Is(err, ErrSetupTokenInvalid) {
		t.Errorf("Setup with a wrong token = %v, want ErrSetupTokenInvalid", err)
	}
}

// TestSetupRaceCreatesExactlyOneAdmin is the regression test for the race this package
// closes with store.CreateFirstAdmin: two concurrent Setup calls, both carrying the one
// valid token, must not both succeed.
func TestSetupRaceCreatesExactlyOneAdmin(t *testing.T) {
	db := testDB(t)
	useFastArgon2Params(t, db)
	b := NewBootstrap(db)
	var buf bytes.Buffer
	if err := b.PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, buf.String())

	const attempts = 8
	var wg sync.WaitGroup
	successes := make([]bool, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Setup(t.Context(), token, "admin-race", "a-fine-password")
			successes[i] = err == nil
		}(i)
	}
	wg.Wait()

	won := 0
	for _, ok := range successes {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent Setup calls succeeded, want exactly 1", won, attempts)
	}

	n, err := db.CountUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("CountUsers = %d after the race, want exactly 1", n)
	}
}
