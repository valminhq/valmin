package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// TestValidJoinHost asserts a join host is an IP address or a DNS name, never something that
// carries a port, a scheme or extra launch arguments.
func TestValidJoinHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want bool
	}{
		{"203.0.113.7", true},
		{"2001:db8::1", true},
		{"play.example.com", true},
		{"valheim", true},
		{"my-host.example", true},
		{strings.Repeat("a", 63) + ".example", true},
		{strings.Repeat("a.", 126) + "a", true},
		{strings.Repeat("a.", 126) + "ab", false},
		{strings.Repeat("a", 64) + ".example", false},
		{"", false},
		{"play.example.com:2456", false},
		{"203.0.113.7:2456", false},
		{"[2001:db8::1]", false},
		{"fe80::1%eth0", false},
		{"https://play.example.com", false},
		{"play.example.com/join", false},
		{"play example.com", false},
		{"play.example.com%20+exec", false},
		{"play.example.com +exec", false},
		{"-play.example.com", false},
		{"play-.example.com", false},
		{"play..example.com", false},
		{"play.example.com.", false},
		{"play_server.example.com", false},
	}
	for _, tt := range tests {
		if got := validJoinHost(tt.host); got != tt.want {
			t.Errorf("validJoinHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// TestJoinAddress asserts the join address pairs the host with the game port, brackets an IPv6
// address, and is absent without a host.
func TestJoinAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want string
	}{
		{"play.example.com", "play.example.com:2456"},
		{"203.0.113.7", "203.0.113.7:2456"},
		{"2001:db8::1", "[2001:db8::1]:2456"},
	}
	for _, tt := range tests {
		if got := deref(joinAddress(tt.host, 2456)); got != tt.want {
			t.Errorf("joinAddress(%q, 2456) = %q, want %q", tt.host, got, tt.want)
		}
	}
	if got := joinAddress("", 2456); got != nil {
		t.Errorf("joinAddress(\"\", 2456) = %q, want nil", *got)
	}
}

// TestSteamJoinURL asserts the Steam link connects to the join address, and is absent without
// one, for an IPv6 address, or with crossplay on.
func TestSteamJoinURL(t *testing.T) {
	t.Parallel()
	addr := "play.example.com:2456"
	if got, want := deref(
		steamJoinURL(&addr, false),
	), "steam://run/892970//+connect%20play.example.com:2456"; got != want {
		t.Errorf("steamJoinURL(%q, false) = %q, want %q", addr, got, want)
	}
	if got := steamJoinURL(&addr, true); got != nil {
		t.Errorf("steamJoinURL(%q, true) = %q, want nil", addr, *got)
	}
	v6 := "[2001:db8::1]:2456"
	if got := steamJoinURL(&v6, false); got != nil {
		t.Errorf("steamJoinURL(%q, false) = %q, want nil", v6, *got)
	}
	if got := steamJoinURL(nil, false); got != nil {
		t.Errorf("steamJoinURL(nil, false) = %q, want nil", *got)
	}
}

// TestPatchJoinHost asserts a valid host is stored trimmed and served as the join address,
// blank clears it, an invalid one is refused with 422 and leaves the row unchanged, and none of
// it requires a restart.
func TestPatchJoinHost(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)

	if rec := patchInstance(
		t,
		rt,
		admin,
		map[string]any{"join_host": " play.example.com "},
	); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.JoinHost != "play.example.com" || inst.RestartRequired {
		t.Fatalf("join_host = %q, restart_required = %v; want the trimmed host and no restart",
			inst.JoinHost, inst.RestartRequired)
	}

	var view struct {
		JoinAddress *string `json:"join_address"`
	}
	decodeInto(t, getInstanceA(t, rt, admin), &view)
	if want := "play.example.com:" + strconv.Itoa(inst.BasePort); deref(view.JoinAddress) != want {
		t.Errorf("join_address = %q, want %q", deref(view.JoinAddress), want)
	}

	if rec := patchInstance(
		t,
		rt,
		admin,
		map[string]any{"join_host": "play.example.com:2456"},
	); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch with a port = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if inst, err = db.InstanceByID(t.Context(), "inst-a"); err != nil {
		t.Fatal(err)
	}
	if inst.JoinHost != "play.example.com" {
		t.Errorf("join_host = %q after a refused patch, want it unchanged", inst.JoinHost)
	}

	if rec := patchInstance(t, rt, admin, map[string]any{"join_host": "  "}); rec.Code != http.StatusOK {
		t.Fatalf("patch blank = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	decodeInto(t, getInstanceA(t, rt, admin), &view)
	if view.JoinAddress != nil {
		t.Errorf("join_address = %q after clearing, want null", *view.JoinAddress)
	}
}

// TestPatchJoinHostIsAudited asserts a change is recorded with its old and new value.
func TestPatchJoinHostIsAudited(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	if rec := patchInstance(t, rt, admin, map[string]any{"join_host": "203.0.113.7"}); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rows := auditRecordsFor(t, db, "instances.settings.update")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	want := `{"changes":[{"field":"join_host","from":"","to":"203.0.113.7"}]}`
	if got := deref(rows[0].Detail); got != want {
		t.Errorf("detail = %s\nwant     %s", got, want)
	}
}

// TestJoinHostNeedsSettings asserts a viewer cannot set the join host.
func TestJoinHostNeedsSettings(t *testing.T) {
	t.Parallel()
	rt, db, _, member := world(t)
	seed(t, db, `UPDATE instance_grants SET role = 'viewer', perms = '[]'
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`)
	rec := patchInstance(t, rt, member, map[string]any{"join_host": "play.example.com"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestExportCodeIsRateLimited asserts each user may upload a few profile codes a minute and
// is refused with 429 beyond that, before the registry is asked.
func TestExportCodeIsRateLimited(t *testing.T) {
	t.Parallel()
	rt, _, admin := exportWorld(t)
	ts := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	rt.mods.Profiles = profileHosts{source.Thunderstore: ts.host}

	for i := range profileCodeBurst {
		if rec := postExportCode(t, rt, admin); rec.Code != http.StatusOK {
			t.Fatalf("upload %d = %d, want 200 (%s)", i+1, rec.Code, rec.Body)
		}
	}
	rec := postExportCode(t, rt, admin)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("upload past the limit = %d, Retry-After %q; want 429 with a wait",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	if len(ts.uploads) != profileCodeBurst {
		t.Errorf("registry uploads = %d, want %d", len(ts.uploads), profileCodeBurst)
	}
}

// getInstanceA is GET /instances/inst-a as u.
func getInstanceA(t *testing.T, rt *Server, u *store.User) *httptest.ResponseRecorder {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	return rec
}
