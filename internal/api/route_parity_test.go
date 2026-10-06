package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The snapshot is the pre-refactor route surface, including timeout and body-limit classes.
const routeParity = `B- GET /api/v1/instances/{id}/admins
B- GET /api/v1/instances/{id}/bans
B- GET /api/v1/instances/{id}/permitted
B- PUT /api/v1/instances/{id}/admins
B- PUT /api/v1/instances/{id}/bans
B- PUT /api/v1/instances/{id}/permitted
B- DELETE /api/v1/admin/alert-rules/{id}
B- DELETE /api/v1/admin/webhooks/{id}
B- DELETE /api/v1/instances/{id}
B- DELETE /api/v1/instances/{id}/backups/{bid}
B- DELETE /api/v1/instances/{id}/grants/{user_id}
B- DELETE /api/v1/instances/{id}/mods/{full_name}
B- DELETE /api/v1/instances/{id}/setups/{sid}
B- DELETE /api/v1/instances/{id}/worlds/{name}
B- DELETE /api/v1/invites/{id}
B- DELETE /api/v1/schedules/{id}
B- DELETE /api/v1/users/{id}
B- GET /api/v1/admin/alert-rules
B- GET /api/v1/admin/diagnostics
B- GET /api/v1/admin/diagnostics/bundle
B- GET /api/v1/admin/remote-backup-destination
B- GET /api/v1/admin/remote-backup-destination/remotes
B- GET /api/v1/admin/webhooks
B- GET /api/v1/admin/webhooks/deliveries
B- GET /api/v1/audit
S- GET /api/v1/audit/export
B- GET /api/v1/audit/filters
B- GET /api/v1/auth/me
B- GET /api/v1/game/options
B- GET /api/v1/instances
B- GET /api/v1/instances/inbox
B- GET /api/v1/instances/orphans
B- GET /api/v1/instances/{id}
B- GET /api/v1/instances/{id}/backups
S- GET /api/v1/instances/{id}/backups/{bid}/download
B- GET /api/v1/instances/{id}/capabilities
B- GET /api/v1/instances/{id}/configs
B- GET /api/v1/instances/{id}/configs/{file}
B- GET /api/v1/instances/{id}/configs/{file}/original
B- GET /api/v1/instances/{id}/configs/{file}/original/raw
B- GET /api/v1/instances/{id}/configs/{file}/previous
B- GET /api/v1/instances/{id}/configs/{file}/previous/raw
B- GET /api/v1/instances/{id}/configs/{file}/raw
B- GET /api/v1/instances/{id}/disk
B- GET /api/v1/instances/{id}/grants
B- GET /api/v1/instances/{id}/grants/{user_id}
B- GET /api/v1/instances/{id}/jobs
B- GET /api/v1/instances/{id}/logs
B- GET /api/v1/instances/{id}/manifest
B- GET /api/v1/instances/{id}/mods
B- GET /api/v1/instances/{id}/mods/export
B- GET /api/v1/instances/{id}/operation
B- GET /api/v1/instances/{id}/password
B- GET /api/v1/instances/{id}/players/history
B- GET /api/v1/instances/{id}/players/seen
B- GET /api/v1/instances/{id}/remote-copies
B- GET /api/v1/instances/{id}/remote-copies/{copy_id}
B- GET /api/v1/instances/{id}/setups
B- GET /api/v1/instances/{id}/setups/{sid}
B- GET /api/v1/instances/{id}/setups/{sid}/preview
B- GET /api/v1/instances/{id}/stats
B- GET /api/v1/instances/{id}/update-status
B- GET /api/v1/instances/{id}/worlds
B- GET /api/v1/invites
B- GET /api/v1/jobs/{id}
B- GET /api/v1/me/permissions
B- GET /api/v1/mods/search
B- GET /api/v1/mods/{namespace}/{name}
B- GET /api/v1/orphans/{container_id}
B- GET /api/v1/schedules
B- GET /api/v1/users
S- GET /api/v1/ws
B- PATCH /api/v1/admin/alert-rules/{id}
B- PATCH /api/v1/admin/webhooks/{id}
B- PATCH /api/v1/instances/{id}
B- PATCH /api/v1/instances/{id}/configs/{file}
B- PATCH /api/v1/instances/{id}/mods/{full_name}
B- PATCH /api/v1/schedules/{id}
B- PATCH /api/v1/users/{id}
B- POST /api/v1/admin/alert-rules
B- POST /api/v1/admin/diagnostics/run
B- POST /api/v1/admin/keys/rotate
B- POST /api/v1/admin/remote-backup-destination/test
B- POST /api/v1/admin/webhooks
B- POST /api/v1/admin/webhooks/{id}/test
B- POST /api/v1/auth/login
B- POST /api/v1/auth/logout
B- POST /api/v1/instances
BL POST /api/v1/instances/import
BL POST /api/v1/instances/manifest/preview
B- POST /api/v1/instances/{id}/acknowledge
B- POST /api/v1/instances/{id}/backups
B- POST /api/v1/instances/{id}/backups/{bid}/remote-copy
B- POST /api/v1/instances/{id}/backups/{bid}/restore
B- POST /api/v1/instances/{id}/clone
B- POST /api/v1/instances/{id}/commands
B- POST /api/v1/instances/{id}/mods
B- POST /api/v1/instances/{id}/mods/resolve
B- POST /api/v1/instances/{id}/mods/updates
B- POST /api/v1/instances/{id}/mods/updates/resolve
B- POST /api/v1/instances/{id}/operation/abandon
B- POST /api/v1/instances/{id}/operation/resume
B- POST /api/v1/instances/{id}/remote-copies/{copy_id}/cancel
B- POST /api/v1/instances/{id}/remote-copies/{copy_id}/retry
B- POST /api/v1/instances/{id}/restart
B- POST /api/v1/instances/{id}/setups
B- POST /api/v1/instances/{id}/setups/{sid}/restore
B- POST /api/v1/instances/{id}/start
B- POST /api/v1/instances/{id}/stop
B- POST /api/v1/instances/{id}/update
SL POST /api/v1/instances/{id}/worlds/import
B- POST /api/v1/instances/{id}/worlds/{name}/restore
B- POST /api/v1/invites
B- POST /api/v1/invites/{token}/redeem
B- POST /api/v1/jobs/{id}/cancel
B- POST /api/v1/me/password
B- POST /api/v1/orphans/{container_id}
B- POST /api/v1/schedules
B- POST /api/v1/setup
B- POST /api/v1/users
B- POST /api/v1/users/{id}/password/reset
B- PUT /api/v1/admin/remote-backup-destination
B- PUT /api/v1/instances/{id}/configs/{file}/raw
B- PUT /api/v1/instances/{id}/grants/{user_id}`

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
