package middleware

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
)

const testOrigin = "https://valmin.example"

// ok records that the request reached the end of the chain.
func ok(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusNoContent)
	})
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return env.Error.Code
}

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestClientIP is 10 §5. The empty-trusted rows are the default deployment, where a
// forwarded header is a spoofing attempt rather than a fact (D9).
func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		trusted []string
		peer    string
		fwd     []string
		want    string
	}{
		{
			name: "no proxies trusted, header ignored",
			peer: "203.0.113.7:5555",
			fwd:  []string{"1.2.3.4"},
			want: "203.0.113.7",
		},
		{
			name:    "trusted proxy, rightmost untrusted entry wins",
			trusted: []string{"172.18.0.0/16"},
			peer:    "172.18.0.2:5555",
			fwd:     []string{"9.9.9.9, 203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "trusted proxy, trailing hops skipped",
			trusted: []string{"172.18.0.0/16", "10.0.0.0/8"},
			peer:    "172.18.0.2:5555",
			fwd:     []string{"203.0.113.7, 10.0.0.5, 10.0.0.6"},
			want:    "203.0.113.7",
		},
		{
			name:    "trusted proxy, every entry trusted, falls back to the peer",
			trusted: []string{"172.18.0.0/16"},
			peer:    "172.18.0.2:5555",
			fwd:     []string{"172.18.0.9"},
			want:    "172.18.0.2",
		},
		{
			name:    "untrusted peer with a trusted-looking header",
			trusted: []string{"172.18.0.0/16"},
			peer:    "203.0.113.7:5555",
			fwd:     []string{"172.18.0.9, 1.2.3.4"},
			want:    "203.0.113.7",
		},
		{
			name:    "several header lines, the last one is walked first",
			trusted: []string{"172.18.0.0/16"},
			peer:    "172.18.0.2:5555",
			fwd:     []string{"1.1.1.1", "203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "v4-mapped peer matches a v4 prefix",
			trusted: []string{"172.18.0.0/16"},
			peer:    "[::ffff:172.18.0.2]:5555",
			fwd:     []string{"203.0.113.7"},
			want:    "203.0.113.7",
		},
		{
			name:    "garbage entries are skipped",
			trusted: []string{"172.18.0.0/16"},
			peer:    "172.18.0.2:5555",
			fwd:     []string{"not-an-ip, 203.0.113.7, also-not-an-ip"},
			want:    "203.0.113.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got netip.Addr
			h := ClientIP(prefixes(t, tt.trusted...))(http.HandlerFunc(
				func(_ http.ResponseWriter, r *http.Request) { got = ClientIPFrom(r.Context()) }))

			r := httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody)
			r.RemoteAddr = tt.peer
			for _, v := range tt.fwd {
				r.Header.Add("X-Forwarded-For", v)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)

			if got.String() != tt.want {
				t.Errorf("client ip = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestOrigin is 11 §6.1. An absent Origin is curl, and curl cannot be a CSRF vector; a
// present one that does not match this panel is rejected before any session is read.
func TestOrigin(t *testing.T) {
	external, err := url.Parse("https://valmin.example:8443/panel")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		headers map[string]string
		allow   bool
	}{
		{name: "no headers at all", allow: true},
		{name: "matching origin", headers: map[string]string{"Origin": "https://valmin.example:8443"}, allow: true},
		{name: "same-origin fetch", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, allow: true},
		{name: "address bar navigation", headers: map[string]string{"Sec-Fetch-Site": "none"}, allow: true},
		{name: "another site", headers: map[string]string{"Origin": "https://evil.example"}},
		{name: "another port", headers: map[string]string{"Origin": "https://valmin.example:9999"}},
		{name: "sandboxed iframe", headers: map[string]string{"Origin": "null"}},
		{name: "cross-site fetch", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{name: "sibling subdomain", headers: map[string]string{"Sec-Fetch-Site": "same-site"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", http.NoBody)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			Origin(external)(ok(&reached)).ServeHTTP(rec, r)

			if reached != tt.allow {
				t.Fatalf("handler reached = %v, want %v", reached, tt.allow)
			}
			if tt.allow {
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if got := codeOf(t, rec); got != "origin_rejected" {
				t.Errorf("code = %q, want origin_rejected", got)
			}
		})
	}
}

// TestNoCORSHeaderEverEscapes holds D3. There is no configuration under which the panel
// emits one, so the assertion is on the layer that sets response headers.
func TestNoCORSHeaderEverEscapes(t *testing.T) {
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/instances", http.NoBody)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	SecurityHeaders(ok(&reached)).ServeHTTP(rec, r)

	for name := range rec.Header() {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
			t.Errorf("response carries %s (D3, ADR-036)", name)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cacheable")
	}
}

func TestBodyLimitRejectsADeclaredOversizeBody(t *testing.T) {
	reached := false
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", bytes.NewReader(make([]byte, 64)))
	BodyLimit(16)(ok(&reached)).ServeHTTP(rec, r)

	if reached {
		t.Error("handler ran; an oversized body must be refused before anything parses it")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if got := codeOf(t, rec); got != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", got)
	}
}

// TestBodyLimitCapsAnUndeclaredBody covers the chunked case, where Content-Length lies or
// is absent and the cap has to bite at the reader.
func TestBodyLimitCapsAnUndeclaredBody(t *testing.T) {
	var readErr error
	h := BodyLimit(16)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		for readErr == nil {
			_, readErr = r.Body.Read(buf)
		}
	}))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/instances", bytes.NewReader(make([]byte, 64)))
	r.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), r)

	var tooLarge *http.MaxBytesError
	if !stderrors.As(readErr, &tooLarge) {
		t.Fatalf("read error = %v, want a MaxBytesError", readErr)
	}
}

func TestRecoverTurnsAPanicIntoTheEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "test-request")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody)

	Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a nil map somewhere")
	})).ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := codeOf(t, rec); got != "internal" {
		t.Errorf("code = %q, want internal", got)
	}
	if strings.Contains(rec.Body.String(), "nil map") {
		t.Error("the panic value reached the caller")
	}
}

// TestRecoverPassesAbortHandlerThrough: net/http's own give-up signal is not a bug, and
// swallowing it would leave the connection half-written instead of closed.
func TestRecoverPassesAbortHandlerThrough(t *testing.T) {
	defer func() {
		v := recover()
		err, _ := v.(error)
		if !stderrors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", v)
		}
	}()
	Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody))
}

func TestRequestIDIsOnEveryResponse(t *testing.T) {
	var fromContext string
	rec := httptest.NewRecorder()
	RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fromContext = RequestIDFrom(r.Context())
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody))

	header := rec.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("no X-Request-Id header")
	}
	if fromContext != header {
		t.Errorf("context id %q and header %q disagree", fromContext, header)
	}
}

// TestLimiterRefusesAndRecovers pins the arithmetic behind Retry-After: 429 always carries
// one (11 §7), and it has to be long enough that obeying it actually succeeds.
func TestLimiterRefusesAndRecovers(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 3)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if allowed, _ := l.Allow("1.2.3.4"); !allowed {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}
	allowed, retry := l.Allow("1.2.3.4")
	if allowed {
		t.Fatal("fourth request allowed; the burst is 3")
	}
	if retry <= 0 {
		t.Fatal("no Retry-After on a refusal")
	}

	if other, _ := l.Allow("5.6.7.8"); !other {
		t.Error("a different key was refused; buckets are per key")
	}

	now = now.Add(retry)
	if allowed, _ := l.Allow("1.2.3.4"); !allowed {
		t.Errorf("still refused after waiting the advertised %s", retry)
	}
}

// TestLimiterTableStaysBounded: the keys are caller-supplied addresses, so an unbounded
// table would be a memory primitive rather than a control.
func TestLimiterTableStaysBounded(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 1)
	l.maxKeys = 8
	l.now = func() time.Time { return now }

	for i := range 100 {
		l.Allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
		now = now.Add(time.Millisecond)
	}
	if len(l.buckets) > l.maxKeys {
		t.Errorf("table holds %d keys, want at most %d", len(l.buckets), l.maxKeys)
	}
}

func testKeeper(t *testing.T) *crypto.Keeper {
	t.Helper()
	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestCSRF is 11 §6.2. The token is derived from the session rather than stored, so it is
// bound to exactly one session and there is no second table to keep in step.
func TestCSRF(t *testing.T) {
	k := testKeeper(t)
	valid, err := CSRFToken(k, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := CSRFToken(k, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if valid == other {
		t.Fatal("two sessions derived the same token")
	}

	tests := []struct {
		name    string
		method  string
		session string
		token   string
		allow   bool
	}{
		{name: "matching token", method: http.MethodPost, session: "session-a", token: valid, allow: true},
		{name: "read is not state changing", method: http.MethodGet, session: "session-a", allow: true},
		{name: "no session to forge against", method: http.MethodPost, allow: true},
		{name: "missing token", method: http.MethodPost, session: "session-a"},
		{name: "another session's token", method: http.MethodPost, session: "session-a", token: other},
		{name: "delete is state changing", method: http.MethodDelete, session: "session-a", token: "nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			r := httptest.NewRequest(tt.method, "/api/v1/instances/abc", http.NoBody)
			if tt.session != "" {
				r = r.WithContext(WithSessionID(r.Context(), tt.session))
			}
			if tt.token != "" {
				r.Header.Set(CSRFHeader, tt.token)
			}
			rec := httptest.NewRecorder()
			CSRF(k)(ok(&reached)).ServeHTTP(rec, r)

			if reached != tt.allow {
				t.Fatalf("handler reached = %v, want %v", reached, tt.allow)
			}
			if !tt.allow {
				if got := codeOf(t, rec); got != "csrf_failed" {
					t.Errorf("code = %q, want csrf_failed", got)
				}
			}
		})
	}
}

// TestCSRFReadReissuesTheCookie is the regression test for the lockout of 3 Sep 2026.
//
// The two cookies of 11 §6.2 had different lifetimes: the session cookie carries the
// session's absolute expiry and survives a browser restart, the CSRF cookie carried none
// and did not. A returning operator therefore held a valid session and no token, so every
// state-changing request answered 403 — **logout and login among them**, which is what made
// it a lockout rather than an inconvenience: neither ending the session nor re-authenticating
// is a GET, so the panel offered no way to clear the state that was causing the failure.
//
// The assertion is the whole loop rather than the Set-Cookie header alone, because the header
// is not the property that matters. What matters is that a read puts the caller back in a
// state where the write that just failed now succeeds.
func TestCSRFReadReissuesTheCookie(t *testing.T) {
	k := testKeeper(t)
	const session = "session-a"

	// The state the operator came back to: session resolves, no token to echo.
	var reached bool
	blocked := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", http.NoBody)
	blocked = blocked.WithContext(WithSessionID(blocked.Context(), session))
	rec := httptest.NewRecorder()
	CSRF(k)(ok(&reached)).ServeHTTP(rec, blocked)
	if reached {
		t.Fatal("a POST with no token reached the handler; the test is not reproducing the lockout")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a rejected write set a cookie; only reads re-assert it")
	}

	// The SPA loads and asks who it is talking to. That read is the way out.
	read := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody)
	read = read.WithContext(WithSessionID(read.Context(), session))
	rec = httptest.NewRecorder()
	CSRF(k)(ok(&reached)).ServeHTTP(rec, read)
	if !reached {
		t.Fatal("an authenticated read was refused")
	}
	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("an authenticated read did not re-assert the CSRF cookie; the lockout has no exit")
	}

	// The same write, now that the browser holds a token again.
	reached = false
	retry := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", http.NoBody)
	retry = retry.WithContext(WithSessionID(retry.Context(), session))
	retry.Header.Set(CSRFHeader, token)
	CSRF(k)(ok(&reached)).ServeHTTP(httptest.NewRecorder(), retry)
	if !reached {
		t.Error("the write still failed after the read re-asserted the cookie")
	}
}

// TestCSRFReadDoesNotReissueWithoutASession guards the one way the fix above could go wrong:
// handing a token to a caller who has not proved they hold the session it is derived from.
func TestCSRFReadDoesNotReissueWithoutASession(t *testing.T) {
	var reached bool
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", http.NoBody)
	CSRF(testKeeper(t))(ok(&reached)).ServeHTTP(rec, r)

	if !reached {
		t.Fatal("an anonymous read was refused")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a request with no session was handed a CSRF cookie")
	}
}

func TestCSRFCookieIsReadableAndStrict(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCSRFCookie(rec, "token")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != CSRFCookie {
		t.Errorf("name = %q, want %q", c.Name, CSRFCookie)
	}
	if c.HttpOnly {
		t.Error("cookie is HttpOnly; the SPA has to read it to echo it back")
	}
	if !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie is not Secure+SameSite=Strict: %+v", c)
	}
}

// TestChainResolvesTheClientIPBeforeRateLimiting is 11 §5.1 rows 3 and 8. Resolving the
// address after the limiter would key every caller on the same empty value, so one
// visitor's traffic would exhaust everybody's budget — and the chain would still look
// correct, because each layer works.
func TestChainResolvesTheClientIPBeforeRateLimiting(t *testing.T) {
	external, err := url.Parse(testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	h := Apply(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), Chain(&Config{
		ExternalURL: external,
		BodyLimit:   1 << 20,
		Keeper:      testKeeper(t),
		PerIP:       NewLimiter(60, time.Minute, 1),
	}))

	for _, peer := range []string{"203.0.113.7:5555", "198.51.100.9:5555"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody)
		r.RemoteAddr = peer
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s got %d on its first request; it is sharing a bucket", peer, rec.Code)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/instances", http.NoBody)
	r.RemoteAddr = "203.0.113.7:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from the same peer got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After (11 §7)")
	}
}
