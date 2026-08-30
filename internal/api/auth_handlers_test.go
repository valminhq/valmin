package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/store"
)

// pendingRouter builds a router the way NewRouter's caller does at a real cold start:
// bootstrapPending computed from an empty database.
func pendingRouter(t *testing.T) (*Router, *store.DB) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Auth.SessionIdleTTL = config.Duration(time.Hour)
	cfg.Auth.SessionAbsoluteTTL = config.Duration(24 * time.Hour)
	cfg.Auth.InviteTTL = config.Duration(7 * 24 * time.Hour)

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)
	fastenArgon2(t, h.DB)

	rt, err := NewRouter(&cfg, h.DB, h, k, true, testEngine(h.DB, &cfg))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return rt, h.DB
}

// fastenArgon2 points the panel at test-speed hashing, or every handler test pays
// Decision 4's real ~64 MiB cost per request.
func fastenArgon2(t *testing.T, db *store.DB) {
	t.Helper()
	fast := struct {
		MemoryKiB uint32 `json:"memory_kib"`
		Time      uint32 `json:"time"`
		Threads   uint8  `json:"threads"`
		SaltLen   uint32 `json:"salt_len"`
		KeyLen    uint32 `json:"key_len"`
	}{MemoryKiB: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := db.KVSet(t.Context(), "argon2_params", fast); err != nil {
		t.Fatal(err)
	}
}

func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}

// cookieValue finds one cookie by name from a response, or "" if absent.
func cookieValue(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// authenticated attaches the session and CSRF cookies a prior response set, and the CSRF
// header a real browser would echo back — the same round trip the SPA's API client makes.
func authenticated(r *http.Request, from *httptest.ResponseRecorder) *http.Request {
	if v := cookieValue(from, "valmin_session"); v != "" {
		r.AddCookie(&http.Cookie{Name: "valmin_session", Value: v})
	}
	if v := cookieValue(from, "valmin_csrf"); v != "" {
		r.AddCookie(&http.Cookie{Name: "valmin_csrf", Value: v})
		r.Header.Set("X-CSRF-Token", v)
	}
	return r
}

// extractPrintedToken finds the token line inside auth.Bootstrap.PrintToken's framed
// output. The frame itself is a long run of "=" with no spaces, which is otherwise the
// same shape as the token (base64 RawURLEncoding: letters, digits, '-', '_') — excluded
// explicitly rather than by a length heuristic that could match either.
func extractPrintedToken(t *testing.T, printed string) string {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 40 && !strings.Contains(line, " ") && strings.Trim(line, "=") != "" {
			return line
		}
	}
	t.Fatalf("no token found in printed output: %q", printed)
	return ""
}

func bootstrapToken(t *testing.T, db *store.DB) string {
	t.Helper()
	var buf bytes.Buffer
	if err := auth.NewBootstrap(db).PrintToken(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	return extractPrintedToken(t, buf.String())
}

// TestBootstrapGateBlocksEverythingButSetup is WP-09's headline acceptance criterion: a
// fresh panel with no users serves 503 setup_required on an ordinary route.
func TestBootstrapGateBlocksEverythingButSetup(t *testing.T) {
	rt, _ := pendingRouter(t)

	rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/me/permissions", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := errCode(t, rec); got != "setup_required" {
		t.Errorf("code = %q, want setup_required", got)
	}
}

func TestSetupCreatesAnAdminAndLogsIn(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)

	rec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	if cookieValue(rec, "valmin_session") == "" || cookieValue(rec, "valmin_csrf") == "" {
		t.Error("setup did not set both cookies")
	}

	// The gate opened: an ordinary route no longer 503s.
	after := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/me/permissions", http.NoBody))
	if after.Code == http.StatusServiceUnavailable {
		t.Error("bootstrap gate is still closed after a successful setup")
	}
}

// TestSetupIsConsumedForever is the 04 §3 / 10 §6 criterion verbatim.
func TestSetupIsConsumedForever(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)

	first := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	if first.Code != http.StatusOK {
		t.Fatalf("first setup = %d (%s)", first.Code, first.Body)
	}

	second := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "bea", "password": "another-password",
	})))
	if second.Code != http.StatusGone {
		t.Errorf("second setup = %d, want 410", second.Code)
	}
	if got := errCode(t, second); got != "setup_consumed" {
		t.Errorf("code = %q, want setup_consumed", got)
	}
}

// TestSetupTokenNeverAppearsInAResponse is D10 applied to the one secret that legitimately
// appears in a log line — it must never also appear in a body.
func TestSetupTokenNeverAppearsInAResponse(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)

	rec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("the setup token leaked into the response body: %s", rec.Body.String())
	}
}

func TestSetupRejectsAWrongToken(t *testing.T) {
	rt, _ := pendingRouter(t)

	rec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": "totally-wrong", "username": "ada", "password": "a-fine-password",
	})))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := errCode(t, rec); got != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", got)
	}
}

func TestLoginAndMe(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)
	send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))

	loginRec := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "ada", "password": "a-fine-password",
	})))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", loginRec.Code, loginRec.Body)
	}

	me := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody), loginRec))
	if me.Code != http.StatusOK {
		t.Fatalf("me = %d (%s), want 200", me.Code, me.Body)
	}
	var got struct {
		Username string `json:"username"`
	}
	decodeInto(t, me, &got)
	if got.Username != "ada" {
		t.Errorf("me.username = %q, want ada", got.Username)
	}

	anon := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody))
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated me = %d, want 401", anon.Code)
	}
}

// TestWrongPasswordAndUnknownUsernameAreIndistinguishable is 11 §2.5's requirement carried
// all the way to the wire.
func TestWrongPasswordAndUnknownUsernameAreIndistinguishable(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)
	send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))

	wrongPassword := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "ada", "password": "not-it",
	})))
	unknownUser := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "nobody", "password": "not-it",
	})))

	if wrongPassword.Code != http.StatusUnauthorized || unknownUser.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, %d, want 401, 401", wrongPassword.Code, unknownUser.Code)
	}
	// Byte-identical envelopes, request_id aside.
	stripID := func(rec *httptest.ResponseRecorder) string {
		var body map[string]any
		decodeInto(t, rec, &body)
		if errObj, ok := body["error"].(map[string]any); ok {
			delete(errObj, "request_id")
		}
		raw, _ := json.Marshal(body)
		return string(raw)
	}
	if stripID(wrongPassword) != stripID(unknownUser) {
		t.Errorf("responses differ: %s vs %s", stripID(wrongPassword), stripID(unknownUser))
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)
	send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	login := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "ada", "password": "a-fine-password",
	})))

	logout := send(rt, authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", http.NoBody), login))
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", logout.Code)
	}

	me := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody), login))
	if me.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", me.Code)
	}
}

// TestStateChangingRequestWithoutCSRFIsRejected is 11 §6.2, exercised end to end: a
// session cookie alone is not enough to act.
func TestStateChangingRequestWithoutCSRFIsRejected(t *testing.T) {
	rt, db := pendingRouter(t)
	token := bootstrapToken(t, db)
	send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/setup", jsonBody(t, map[string]string{
		"token": token, "username": "ada", "password": "a-fine-password",
	})))
	login := send(rt, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", jsonBody(t, map[string]string{
		"username": "ada", "password": "a-fine-password",
	})))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", http.NoBody)
	r.AddCookie(&http.Cookie{Name: "valmin_session", Value: cookieValue(login, "valmin_session")})
	// Deliberately no CSRF cookie or header.
	rec := send(rt, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := errCode(t, rec); got != "csrf_failed" {
		t.Errorf("code = %q, want csrf_failed", got)
	}
}
