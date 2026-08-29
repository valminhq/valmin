package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
)

const testOrigin = "https://valmin.example"

func router(t *testing.T) *Router {
	t.Helper()

	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Server.BodyLimitBytes = 64
	cfg.Server.RequestTimeout = config.Duration(100 * time.Millisecond)

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)

	rt, err := NewRouter(&cfg, h, k)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return rt
}

// send runs one request through the whole surface, same-origin unless a test says otherwise.
func send(rt *Router, r *http.Request) *httptest.ResponseRecorder {
	if r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") == "" {
		r.Header.Set("Origin", testOrigin)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, r)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
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

// TestUnmatchedAPIPathIsJSON is G4. A mistyped API path answering with index.html and 200
// fails inside the frontend's JSON parser with an error that names neither the URL nor the
// real problem, and hours disappear into it.
func TestUnmatchedAPIPathIsJSON(t *testing.T) {
	rt := router(t)
	rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", http.NoBody))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := errCode(t, rec); got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}
}

// TestSPAFallbackCannotSwallowAPI holds the structural half of G4: /api/ is registered, so
// http.ServeMux prefers it over any later "/" that serves the SPA.
func TestSPAFallbackCannotSwallowAPI(t *testing.T) {
	rt := router(t)
	rt.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>SPA</title>"))
	}))

	// Unversioned too: the guard is the /api prefix, not /api/v1, or a client that drops
	// the version gets HTML and a parser error that names neither.
	for _, path := range []string{"/api/v1/typo", "/api/typo", "/api"} {
		rec := send(rt, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if strings.Contains(rec.Body.String(), "doctype") {
			t.Fatalf("the SPA fallback caught %s: %s", path, rec.Body.String())
		}
	}

	if page := send(
		rt,
		httptest.NewRequest(http.MethodGet, "/instances/abc", http.NoBody),
	); page.Code != http.StatusOK {
		t.Errorf("client-side route = %d, want the SPA to serve it", page.Code)
	}
}

// TestNoCORSHeaderOnAnyRoute is D3, asserted across a success, a rejection and a probe:
// there is no configuration under which the panel emits one.
func TestNoCORSHeaderOnAnyRoute(t *testing.T) {
	rt := router(t)
	rt.Handle("GET /api/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/thing", http.NoBody),
		httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", http.NoBody),
		httptest.NewRequest(http.MethodOptions, "/api/v1/thing", http.NoBody),
		httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody),
	} {
		rec := send(rt, req)
		for name := range rec.Header() {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
				t.Errorf("%s %s carries %s (D3, ADR-036)", req.Method, req.URL.Path, name)
			}
		}
	}
}

// TestOriginIsCheckedBeforeCSRF is the ordering half of 11 §5.1 rows 6 and 10. A request
// that is wrong in both ways must name the outer failure, which proves the origin check
// runs before anything reads a session — and CSRF is bound to the session (row 9).
func TestOriginIsCheckedBeforeCSRF(t *testing.T) {
	rt := router(t)
	reached := false
	rt.Handle("POST /api/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/thing", http.NoBody)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("X-CSRF-Token", "also-wrong")
	rec := send(rt, r)

	if reached {
		t.Fatal("the handler ran on a cross-site request")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := errCode(t, rec); got != "origin_rejected" {
		t.Errorf("code = %q, want origin_rejected before any session work", got)
	}
}

// TestBodyLimitRunsBeforeTheOriginCheck is 11 §5.1 rows 5 and 6: the body is capped before
// anything below it can buffer or parse it.
func TestBodyLimitRunsBeforeTheOriginCheck(t *testing.T) {
	rt := router(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/thing", bytes.NewReader(make([]byte, 4096)))
	r.Header.Set("Origin", "https://evil.example")

	if got := errCode(t, send(rt, r)); got != "payload_too_large" {
		t.Errorf("code = %q, want payload_too_large", got)
	}
}

// TestForwardedHeaderDoesNotSetTheRateLimitKey is 10 §5 seen through the layer that
// consumes it: with trusted_proxies empty, a caller cannot spend someone else's budget or
// escape their own by rotating a header (D9).
func TestForwardedHeaderDoesNotSetTheRateLimitKey(t *testing.T) {
	rt := router(t)
	rt.Handle("GET /api/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, r, http.StatusOK, map[string]string{"ip": middleware.ClientIPFrom(r.Context()).String()})
	}))

	for _, spoof := range []string{"1.2.3.4", "5.6.7.8"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/thing", http.NoBody)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("X-Forwarded-For", spoof)

		var got struct {
			IP string `json:"ip"`
		}
		rec := send(rt, r)
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.IP != "203.0.113.7" {
			t.Errorf("with X-Forwarded-For %s the panel recorded %s, want the socket peer", spoof, got.IP)
		}
	}
}

// TestPanicIsAnEnvelopeWithARequestID is 11 §5.1 row 1 sitting outside row 2: a panic below
// still answers in the envelope, and still carries the id that ties it to the log line.
func TestPanicIsAnEnvelopeWithARequestID(t *testing.T) {
	rt := router(t)
	rt.Handle("GET /api/v1/boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a nil map somewhere")
	}))

	rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/boom", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := errCode(t, rec); got != "internal" {
		t.Errorf("code = %q, want internal", got)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id on a panic response")
	}
	if id := errRequestID(t, rec); id != rec.Header().Get("X-Request-Id") {
		t.Errorf("envelope request_id %q and header %q disagree", id, rec.Header().Get("X-Request-Id"))
	}
}

func errRequestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Error.RequestID
}

// TestRequestIDOnEveryResponse holds 11 §2.1: success or failure, an operator can tie what
// they saw to the log line that has the real error in it.
func TestRequestIDOnEveryResponse(t *testing.T) {
	rt := router(t)
	rt.Handle("GET /api/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/api/v1/thing", "/api/v1/nonexistent"} {
		rec := send(rt, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Header().Get("X-Request-Id") == "" {
			t.Errorf("%s carries no X-Request-Id", path)
		}
	}
}

// TestStreamRouteOutlivesTheRequestTimeout is C12. A handler on a normal route is cut off
// at server.request_timeout; a streaming route is not, because a write deadline severs the
// console and presents as "the console randomly disconnects".
func TestStreamRouteOutlivesTheRequestTimeout(t *testing.T) {
	rt := router(t) // request timeout is 100ms
	const overrun = 300 * time.Millisecond

	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(overrun)
		w.WriteHeader(http.StatusNoContent)
	})
	rt.Handle("GET /api/v1/slow", slow)
	rt.Stream("GET /api/v1/ws", slow)

	if rec := send(
		rt,
		httptest.NewRequest(http.MethodGet, "/api/v1/slow", http.NoBody),
	); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("normal route = %d, want the request timeout to fire (503)", rec.Code)
	}

	rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/ws", http.NoBody))
	if rec.Code != http.StatusNoContent {
		t.Errorf("stream route = %d, want the handler to finish uninterrupted", rec.Code)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("stream route did not disable proxy buffering (11 §8.1.1)")
	}
}

// TestTimeoutBodyIsTheEnvelope: http.TimeoutHandler writes text/plain by default, and
// 11 §1.1 has no endpoint that fails as a bare string.
func TestTimeoutBodyIsTheEnvelope(t *testing.T) {
	rt := router(t)
	rt.Handle("GET /api/v1/slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := send(rt, httptest.NewRequest(http.MethodGet, "/api/v1/slow", http.NoBody))
	if got := errCode(t, rec); got != "unavailable" {
		t.Errorf("code = %q, want unavailable", got)
	}
}

// TestProbesBypassTheChain is G5. A health check that trips the rate limiter or the origin
// check takes the panel down for the orchestrator that was only asking if it was alive.
func TestProbesBypassTheChain(t *testing.T) {
	rt := router(t)
	r := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	r.Header.Set("Origin", "https://evil.example")

	if rec := send(rt, r); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d for a probe with no idea what origin it should claim", rec.Code)
	}
}
