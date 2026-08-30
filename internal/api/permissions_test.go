package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/store"
)

// as sends r through the whole surface with u already authenticated, standing in for the
// session layer of 11 §5.1 row 9.
func as(rt *Router, u *store.User, r *http.Request) *httptest.ResponseRecorder {
	return send(rt, r.WithContext(middleware.WithUser(r.Context(), u)))
}

func seed(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Writer.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// world sets up the shape 09 §6 describes: an admin, a member, two instances, and a grant
// on exactly one of them.
func world(t *testing.T) (rt *Router, db *store.DB, adminUser, memberUser *store.User) {
	t.Helper()
	rt, db = routerWithDB(t)

	for _, u := range []struct {
		id, name string
		role     store.Role
	}{{"u-admin", "ada", store.RoleAdmin}, {"u-member", "mel", store.RoleMember}} {
		seed(t, db, `INSERT INTO users (id, username, password_hash, role, created_at)
			VALUES (?, ?, 'argon2id$stub', ?, ?)`, u.id, u.name, string(u.role), store.Now())
	}
	for i, id := range []string{"inst-a", "inst-b"} {
		seed(t, db, `INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, ?, ?, ?, 'v1.k.n.ct', ?, ?, ?)`,
			id, id, "/srv/valmin/instances/"+id, 2456+i*5,
			"Server "+id, "World"+id, "cp-"+id, store.Now(), store.Now())
	}
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-member', 'inst-a', 'viewer', '[]', ?)`, store.Now())

	return rt, db,
		&store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin},
		&store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}
}

// TestInvisibleInstanceIsNotFound is D2 / ADR-038, and it is the criterion this package
// exists for. A 403 on an instance id is an existence oracle: a member holds nothing by
// default, so iterating ids would map every world on the panel, including the names of the
// ones they were deliberately not given.
func TestInvisibleInstanceIsNotFound(t *testing.T) {
	rt, _, _, mel := world(t)

	rec := as(rt, mel, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-b/capabilities", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (a 403 would confirm inst-b exists)", rec.Code)
	}
	if got := errCode(t, rec); got != "not_found" {
		t.Errorf("code = %q, want not_found", got)
	}

	// Byte-identical to an instance that genuinely does not exist, or the pair of answers
	// is the oracle all over again.
	missing := as(rt, mel, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-nope/capabilities", http.NoBody))
	if missing.Code != rec.Code {
		t.Errorf("invisible = %d, nonexistent = %d; the two must not be distinguishable",
			rec.Code, missing.Code)
	}
}

// The wire shapes, read back as plain strings. authz.Action deliberately has no
// UnmarshalJSON: a closed registry that could be rebuilt from an arbitrary string would
// not be closed.
type wireCapabilities struct {
	CommandChannel  string   `json:"command_channel"`
	Detected        bool     `json:"detected"`
	AllowedCommands []string `json:"allowed_commands"`
	AllowedActions  []string `json:"allowed_actions"`
}

type wireInstance struct {
	InstanceID     string   `json:"instance_id"`
	AllowedActions []string `json:"allowed_actions"`
}

type wireMine struct {
	UserID    string         `json:"user_id"`
	Role      string         `json:"role"`
	Instances []wireInstance `json:"instances"`
}

func decodeCapabilities(t *testing.T, rec *httptest.ResponseRecorder) wireCapabilities {
	t.Helper()
	var got wireCapabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return got
}

// TestCapabilitiesCarriesAllowedActions is 09 §4.2: the SPA renders from this list and
// never from a role name (F3).
func TestCapabilitiesCarriesAllowedActions(t *testing.T) {
	rt, _, ada, mel := world(t)

	rec := as(rt, mel, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/capabilities", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	got := decodeCapabilities(t, rec)

	names := got.AllowedActions
	for _, want := range []string{"instance.view", "console.read", "stats.read", "config.read"} {
		if !slices.Contains(names, want) {
			t.Errorf("viewer's allowed_actions %v is missing %s", names, want)
		}
	}
	for _, forbidden := range []string{"instance.start", "instance.delete", "mods.manage"} {
		if slices.Contains(names, forbidden) {
			t.Errorf("viewer's allowed_actions leaked %s", forbidden)
		}
	}

	// The command channel resolves to none on this build: strace showed zero reads on
	// fd 0 (E3, 03 §7). The probe that would set detected is M2.
	if got.CommandChannel != "none" {
		t.Errorf("command_channel = %q, want none (E3)", got.CommandChannel)
	}
	if got.Detected {
		t.Error("detected is true, but no probe has run on this build (07 §8)")
	}

	adminRec := as(rt, ada, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-b/capabilities", http.NoBody))
	if adminRec.Code != http.StatusOK {
		t.Errorf("admin got %d on inst-b, want 200", adminRec.Code)
	}
}

func decodeMine(t *testing.T, rec *httptest.ResponseRecorder) wireMine {
	t.Helper()
	var got wireMine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return got
}

// TestMyPermissionsListsOnlyWhatTheCallerHolds is 04 §3's endpoint and 09 §1's promise: a
// member has an empty dashboard until granted.
func TestMyPermissionsListsOnlyWhatTheCallerHolds(t *testing.T) {
	rt, _, ada, mel := world(t)

	got := decodeMine(t, as(rt, mel, httptest.NewRequest(http.MethodGet, "/api/v1/me/permissions", http.NoBody)))
	if len(got.Instances) != 1 || got.Instances[0].InstanceID != "inst-a" {
		t.Fatalf("member sees %+v, want only inst-a", got.Instances)
	}
	if got.Role != string(store.RoleMember) {
		t.Errorf("role = %q, want member", got.Role)
	}

	adminView := decodeMine(t, as(rt, ada, httptest.NewRequest(http.MethodGet,
		"/api/v1/me/permissions", http.NoBody)))
	if len(adminView.Instances) != 2 {
		t.Errorf("admin sees %d instances, want both", len(adminView.Instances))
	}
}

// TestExpiredGrantDisappearsFromThePayload carries D11 all the way to the wire: enforcement
// was never deferred, only the UI for setting an expiry was (Q10).
func TestExpiredGrantDisappearsFromThePayload(t *testing.T) {
	rt, db, _, mel := world(t)
	seed(t, db, `UPDATE instance_grants SET expires_at = ? WHERE user_id = 'u-member'`,
		store.FormatTime(time.Now().Add(-time.Second)))

	rec := as(rt, mel, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/capabilities", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d on an expired grant, want 404", rec.Code)
	}

	got := decodeMine(t, as(rt, mel, httptest.NewRequest(http.MethodGet,
		"/api/v1/me/permissions", http.NoBody)))
	if len(got.Instances) != 0 {
		t.Errorf("an expired grant still appears: %+v", got.Instances)
	}
}

// TestUnauthenticatedIsRejected: the chain resolves who you are, and a route that requires
// a user still checks, because a route reachable without one answers for nobody.
func TestUnauthenticatedIsRejected(t *testing.T) {
	rt, _, _, _ := world(t)

	for _, path := range []string{"/api/v1/me/permissions", "/api/v1/instances/inst-a/capabilities"} {
		rec := send(rt, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d without a session, want 401", path, rec.Code)
		}
		if got := errCode(t, rec); got != "unauthenticated" {
			t.Errorf("%s code = %q, want unauthenticated", path, got)
		}
	}
}

// TestDisabledUserKeepsNothing: disabling an account has to take effect at the seam, not
// only at login, or a live session outlives the decision.
func TestDisabledUserKeepsNothing(t *testing.T) {
	rt, _, ada, _ := world(t)
	ada.Disabled = true

	rec := as(rt, ada, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/capabilities", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a disabled admin got %d, want 404", rec.Code)
	}
}
