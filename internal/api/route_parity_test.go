package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// routeParity is every route registered behind the API chain, sorted by path. Each line starts
// with its class: B for a buffered response or S for a stream, then L for the large body limit
// or - for the default one.
const routeParity = `B- GET /api/v1/admin/alert-rules
B- POST /api/v1/admin/alert-rules
B- DELETE /api/v1/admin/alert-rules/{id}
B- PATCH /api/v1/admin/alert-rules/{id}
B- GET /api/v1/admin/diagnostics
B- GET /api/v1/admin/diagnostics/bundle
B- POST /api/v1/admin/diagnostics/run
B- DELETE /api/v1/admin/discord
B- GET /api/v1/admin/discord
B- PUT /api/v1/admin/discord
B- POST /api/v1/admin/keys/rotate
B- GET /api/v1/admin/remote-backup-destination
B- PUT /api/v1/admin/remote-backup-destination
B- GET /api/v1/admin/remote-backup-destination/remotes
B- POST /api/v1/admin/remote-backup-destination/test
B- GET /api/v1/admin/shutdowns
B- POST /api/v1/admin/shutdowns
B- DELETE /api/v1/admin/shutdowns/{id}
B- GET /api/v1/admin/webhooks
B- POST /api/v1/admin/webhooks
B- GET /api/v1/admin/webhooks/deliveries
B- DELETE /api/v1/admin/webhooks/{id}
B- PATCH /api/v1/admin/webhooks/{id}
B- POST /api/v1/admin/webhooks/{id}/test
B- GET /api/v1/audit
S- GET /api/v1/audit/export
B- GET /api/v1/audit/filters
B- POST /api/v1/auth/login
B- POST /api/v1/auth/logout
B- GET /api/v1/auth/me
B- GET /api/v1/game/options
B- GET /api/v1/instances
B- POST /api/v1/instances
BL POST /api/v1/instances/import
B- GET /api/v1/instances/inbox
BL POST /api/v1/instances/manifest/preview
B- GET /api/v1/instances/orphans
B- DELETE /api/v1/instances/{id}
B- GET /api/v1/instances/{id}
B- PATCH /api/v1/instances/{id}
B- POST /api/v1/instances/{id}/acknowledge
B- GET /api/v1/instances/{id}/admins
B- PUT /api/v1/instances/{id}/admins
B- GET /api/v1/instances/{id}/backups
B- POST /api/v1/instances/{id}/backups
B- DELETE /api/v1/instances/{id}/backups/{bid}
S- GET /api/v1/instances/{id}/backups/{bid}/download
B- POST /api/v1/instances/{id}/backups/{bid}/remote-copy
B- POST /api/v1/instances/{id}/backups/{bid}/restore
B- GET /api/v1/instances/{id}/bans
B- PUT /api/v1/instances/{id}/bans
B- GET /api/v1/instances/{id}/capabilities
B- POST /api/v1/instances/{id}/clone
B- POST /api/v1/instances/{id}/commands
B- POST /api/v1/instances/{id}/world-tools
B- GET /api/v1/instances/{id}/configs
B- GET /api/v1/instances/{id}/configs/{file}
B- PATCH /api/v1/instances/{id}/configs/{file}
B- DELETE /api/v1/instances/{id}/configs/{file}
B- GET /api/v1/instances/{id}/configs/{file}/original
B- GET /api/v1/instances/{id}/configs/{file}/original/raw
B- GET /api/v1/instances/{id}/configs/{file}/previous
B- GET /api/v1/instances/{id}/configs/{file}/previous/raw
B- GET /api/v1/instances/{id}/configs/{file}/raw
B- PUT /api/v1/instances/{id}/configs/{file}/raw
B- GET /api/v1/instances/{id}/disk
B- GET /api/v1/instances/{id}/grants
B- DELETE /api/v1/instances/{id}/grants/{user_id}
B- GET /api/v1/instances/{id}/grants/{user_id}
B- PUT /api/v1/instances/{id}/grants/{user_id}
B- GET /api/v1/instances/{id}/jobs
B- GET /api/v1/instances/{id}/logs
B- GET /api/v1/instances/{id}/manifest
B- GET /api/v1/instances/{id}/manifest/code
B- GET /api/v1/instances/{id}/mods
B- POST /api/v1/instances/{id}/mods
B- GET /api/v1/instances/{id}/mods/export
B- GET /api/v1/instances/{id}/mods/queue
B- POST /api/v1/instances/{id}/mods/queue
B- DELETE /api/v1/instances/{id}/mods/queue/{full_name}
B- POST /api/v1/instances/{id}/mods/resolve
B- POST /api/v1/instances/{id}/mods/updates
B- POST /api/v1/instances/{id}/mods/updates/queue
B- POST /api/v1/instances/{id}/mods/updates/resolve
B- DELETE /api/v1/instances/{id}/mods/{full_name}
B- PATCH /api/v1/instances/{id}/mods/{full_name}
B- GET /api/v1/instances/{id}/operation
B- POST /api/v1/instances/{id}/operation/abandon
B- POST /api/v1/instances/{id}/operation/resume
B- GET /api/v1/instances/{id}/password
B- GET /api/v1/instances/{id}/permitted
B- PUT /api/v1/instances/{id}/permitted
B- GET /api/v1/instances/{id}/players/history
B- GET /api/v1/instances/{id}/players/seen
B- GET /api/v1/instances/{id}/remote-copies
B- GET /api/v1/instances/{id}/remote-copies/{copy_id}
B- POST /api/v1/instances/{id}/remote-copies/{copy_id}/cancel
B- POST /api/v1/instances/{id}/remote-copies/{copy_id}/retry
B- POST /api/v1/instances/{id}/restart
B- GET /api/v1/instances/{id}/setups
B- POST /api/v1/instances/{id}/setups
B- DELETE /api/v1/instances/{id}/setups/{sid}
B- GET /api/v1/instances/{id}/setups/{sid}
B- GET /api/v1/instances/{id}/setups/{sid}/preview
B- POST /api/v1/instances/{id}/setups/{sid}/restore
B- POST /api/v1/instances/{id}/start
B- GET /api/v1/instances/{id}/stats
B- POST /api/v1/instances/{id}/stop
B- POST /api/v1/instances/{id}/update
B- GET /api/v1/instances/{id}/update-status
B- GET /api/v1/instances/{id}/worlds
SL POST /api/v1/instances/{id}/worlds/import
B- DELETE /api/v1/instances/{id}/worlds/{name}
B- POST /api/v1/instances/{id}/worlds/{name}/restore
B- GET /api/v1/invites
B- POST /api/v1/invites
B- DELETE /api/v1/invites/{id}
B- POST /api/v1/invites/{token}/redeem
B- GET /api/v1/jobs/{id}
B- POST /api/v1/jobs/{id}/cancel
B- PATCH /api/v1/me
B- POST /api/v1/me/password
B- GET /api/v1/me/permissions
B- GET /api/v1/mods/search
B- GET /api/v1/mods/{namespace}/{name}
B- GET /api/v1/orphans/{container_id}
B- POST /api/v1/orphans/{container_id}
B- GET /api/v1/schedules
B- POST /api/v1/schedules
B- DELETE /api/v1/schedules/{id}
B- PATCH /api/v1/schedules/{id}
B- POST /api/v1/setup
B- GET /api/v1/users
B- POST /api/v1/users
B- DELETE /api/v1/users/{id}
B- PATCH /api/v1/users/{id}
B- POST /api/v1/users/{id}/password/reset
S- GET /api/v1/ws`

// TestRouteParity asserts that the API registers exactly the routes in routeParity, each with
// its class, and that each pattern resolves to itself.
func TestRouteParity(t *testing.T) {
	srv := router(t)
	want := map[string]string{}
	for line := range strings.SplitSeq(routeParity, "\n") {
		want[line[3:]] = line[:2]
	}
	got := map[string]string{}
	for _, route := range srv.routes {
		flags := "B"
		if route.stream {
			flags = "S"
		}
		if route.largeBody {
			flags += "L"
		} else {
			flags += "-"
		}
		if _, exists := got[route.pattern]; exists {
			t.Errorf("duplicate route %s", route.pattern)
		}
		got[route.pattern] = flags
		if _, pattern := srv.router.api.Handler(requestForPattern(route.pattern)); pattern != route.pattern {
			t.Errorf("route %s matches %q", route.pattern, pattern)
		}
	}
	for pattern, flags := range want {
		if got[pattern] != flags {
			t.Errorf("route %s = %q, want %q", pattern, got[pattern], flags)
		}
	}
	for pattern := range got {
		if _, ok := want[pattern]; !ok {
			t.Errorf("new route %s", pattern)
		}
	}
}

func requestForPattern(pattern string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	path = wildcardPattern.ReplaceAllString(path, "sample")
	return httptest.NewRequest(method, path, http.NoBody)
}

var wildcardPattern = regexp.MustCompile(`\{[^}]+\}`)

// TestMuxRoutesOutsideTheAPIChain asserts the routes registered directly on the outer mux: the
// probes, the public status page, the API subtree and the SPA fallback.
func TestMuxRoutesOutsideTheAPIChain(t *testing.T) {
	srv := router(t)
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodGet, "/healthz", "GET /healthz"},
		{http.MethodGet, "/readyz", "GET /readyz"},
		{http.MethodGet, "/public/status/sample", "GET /public/status/{id}"},
		{http.MethodGet, "/api/v1/anything", "/api/"},
		{http.MethodGet, "/instances/sample", "/"},
	} {
		if _, pattern := srv.router.mux.Handler(
			httptest.NewRequest(tc.method, tc.path, http.NoBody),
		); pattern != tc.want {
			t.Errorf("%s %s matches %q, want %q", tc.method, tc.path, pattern, tc.want)
		}
	}
}
