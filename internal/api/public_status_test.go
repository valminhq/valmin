package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func statusPath(id string) string { return "/public/status/" + id }

// public sends a request the way a stranger would: no session, no CSRF token, no Origin.
func public(rt *Router, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return rec
}

// TestANewInstanceIsPrivate is the default that makes this feature safe to ship at all: a
// panel that published on creation would put every server's name on the internet until
// somebody noticed (05 M5).
func TestANewInstanceIsPrivate(t *testing.T) {
	rt, _, _, _ := world(t)
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// TestUnpublishedAndMissingAreTheSameAnswer. On an internet-facing route a distinguishable
// "exists but private" is an existence oracle for every world on the panel (ADR-038, D2).
func TestUnpublishedAndMissingAreTheSameAnswer(t *testing.T) {
	rt, _, _, _ := world(t)

	private := public(rt, statusPath("inst-a"))
	missing := public(rt, statusPath("no-such-instance"))
	if private.Code != missing.Code {
		t.Fatalf("private = %d, missing = %d; they must be indistinguishable", private.Code, missing.Code)
	}
	// Bodies differ only in the request id every response carries.
	var privateErr, missingErr struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	decodeInto(t, private, &privateErr)
	decodeInto(t, missing, &missingErr)
	if privateErr.Error != missingErr.Error {
		t.Errorf("private %+v differs from missing %+v", privateErr.Error, missingErr.Error)
	}
}

// TestPublishingIsAnAdminDecision. The column is the authorization for the public route, so
// who may set it is the whole access-control story (ADR-156).
func TestPublishingIsAnAdminDecision(t *testing.T) {
	rt, db, admin, member := world(t)

	memberRec := as(rt, member, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true})))
	if memberRec.Code != http.StatusForbidden {
		t.Fatalf("member published = %d, want 403 (%s)", memberRec.Code, memberRec.Body)
	}
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Fatalf("refused publish still exposed the instance: %d", rec.Code)
	}

	adminRec := as(rt, admin, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true})))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin publish = %d, want 200 (%s)", adminRec.Code, adminRec.Body)
	}
	var updated struct {
		StatusPublished bool `json:"status_published"`
		RestartRequired bool `json:"restart_required"`
	}
	decodeInto(t, adminRec, &updated)
	if !updated.StatusPublished {
		t.Error("status_published did not stick")
	}
	// It shapes no container, so telling the operator to restart for it would be false.
	if updated.RestartRequired {
		t.Error("publishing set restart_required")
	}

	rec := public(rt, statusPath("inst-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("published instance = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE action = 'instances.status.published'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("audit rows for the publication = %d, want 1", count)
	}
}

// TestUnpublishingTakesItBack. The toggle has to work in both directions or the operator
// cannot undo a mistake.
func TestUnpublishingTakesItBack(t *testing.T) {
	rt, _, admin, _ := world(t)
	for _, on := range []bool{true, false} {
		rec := as(rt, admin, httptest.NewRequest(
			http.MethodPatch, "/api/v1/instances/inst-a",
			jsonBody(t, map[string]any{"status_published": on})))
		if rec.Code != http.StatusOK {
			t.Fatalf("set published=%v = %d (%s)", on, rec.Code, rec.Body)
		}
	}
	if rec := public(rt, statusPath("inst-a")); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished instance still readable: %d (%s)", rec.Code, rec.Body)
	}
}

// TestThePublicPayloadCarriesNothingElse pins the contract. Every field on this route is a
// deliberate disclosure to strangers, so the test names what must never appear rather than
// only what must.
func TestThePublicPayloadCarriesNothingElse(t *testing.T) {
	rt, _, admin, _ := world(t)
	if rec := as(rt, admin, httptest.NewRequest(
		http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"status_published": true}))); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d (%s)", rec.Code, rec.Body)
	}

	rec := public(rt, statusPath("inst-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	var got struct {
		Name    string `json:"name"`
		Online  bool   `json:"online"`
		Players *int   `json:"players"`
	}
	decodeInto(t, rec, &got)
	if got.Name != "Server inst-a" {
		t.Errorf("name = %q, want the server name", got.Name)
	}
	if got.Online {
		t.Error("a stopped instance reported online")
	}
	// Never zero for a server nobody is reading the log of: that is E7's lie, and on a public
	// page it tells strangers the server is empty.
	if got.Players != nil {
		t.Errorf("players = %d, want null", *got.Players)
	}

	for _, leak := range []string{
		"data_dir", "/srv/valmin", "password", "base_port", "2456", "crossplay", "cp-inst-a",
		"world", "state", "container", "grant",
	} {
		if bytes.Contains(bytes.ToLower(rec.Body.Bytes()), []byte(leak)) {
			t.Errorf("public payload mentions %q: %s", leak, rec.Body)
		}
	}
}

// TestThePublicRouteHasItsOwnLimit. It is the only route a stranger can reach, and it must
// not spend the budget the login route shares (11 §7).
func TestThePublicRouteHasItsOwnLimit(t *testing.T) {
	rt, _, _, _ := world(t)

	var limited bool
	for range publicStatusBurst + 5 {
		if public(rt, statusPath("inst-a")).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the public route never rate limited")
	}
	// The chain-wide limiter is a different bucket, so an authenticated caller is unaffected.
	if rec := public(rt, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("the public limiter reached an unrelated route: %d", rec.Code)
	}
}

// TestThePublicRouteCarriesSecurityHeaders is what separates it from the health probes, which
// are registered on the bare mux and would be the wrong seam to reuse (11 §10).
func TestThePublicRouteCarriesSecurityHeaders(t *testing.T) {
	rt, _, _, _ := world(t)
	rec := public(rt, statusPath("inst-a"))
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("no request id; an operator cannot tie a report to a log line")
	}
}
