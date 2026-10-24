package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Access tiers in the route tables. Any other value is a space-separated list of instance
// actions, each of which on its own lets a viewer through.
const (
	accessView   = "view"   // the viewer base set
	accessMember = "member" // any signed-in user
	accessPublic = "public" // no session needed
	accessAdmin  = "admin"  // admin only; a member is refused with 403
	accessHidden = "hidden" // admin only; a member is answered 404, as if the route were not there
	accessSkip   = "skip"   // not exercised here
)

// instanceRouteAccess is what a member's grant must carry for each route under
// /api/v1/instances/{id}, given the request routeAccessRequest sends: no query and an empty
// JSON object for a body. A route whose action depends on the body (PATCH of the instance,
// commands) is listed by what that empty request needs; its own tests cover the rest.
var instanceRouteAccess = map[string]string{
	"GET /api/v1/instances/{id}":                                 accessView,
	"PATCH /api/v1/instances/{id}":                               accessView,
	"DELETE /api/v1/instances/{id}":                              accessAdmin,
	"POST /api/v1/instances/{id}/acknowledge":                    "instance.start",
	"GET /api/v1/instances/{id}/admins":                          "players.manage",
	"PUT /api/v1/instances/{id}/admins":                          "players.manage",
	"GET /api/v1/instances/{id}/alert-rules":                     "notifications.manage",
	"POST /api/v1/instances/{id}/alert-rules":                    "notifications.manage",
	"POST /api/v1/instances/{id}/alert-rules/test":               "notifications.manage",
	"DELETE /api/v1/instances/{id}/alert-rules/{rid}":            "notifications.manage",
	"PATCH /api/v1/instances/{id}/alert-rules/{rid}":             "notifications.manage",
	"GET /api/v1/instances/{id}/backups":                         accessView,
	"POST /api/v1/instances/{id}/backups":                        "backups.create",
	"DELETE /api/v1/instances/{id}/backups/{bid}":                "backups.restore",
	"GET /api/v1/instances/{id}/backups/{bid}/download":          "backups.download",
	"POST /api/v1/instances/{id}/backups/{bid}/remote-copy":      "backups.create",
	"POST /api/v1/instances/{id}/backups/{bid}/restore":          "backups.restore",
	"GET /api/v1/instances/{id}/bans":                            "players.manage",
	"PUT /api/v1/instances/{id}/bans":                            "players.manage",
	"GET /api/v1/instances/{id}/capabilities":                    accessView,
	"POST /api/v1/instances/{id}/clone":                          accessAdmin,
	"POST /api/v1/instances/{id}/commands":                       "commands.send",
	"POST /api/v1/instances/{id}/world-tools":                    accessAdmin,
	"GET /api/v1/instances/{id}/configs":                         accessView,
	"GET /api/v1/instances/{id}/configs/{file}":                  accessView,
	"PATCH /api/v1/instances/{id}/configs/{file}":                "config.edit config.raw",
	"DELETE /api/v1/instances/{id}/configs/{file}":               "config.edit config.raw",
	"GET /api/v1/instances/{id}/configs/{file}/original":         accessView,
	"GET /api/v1/instances/{id}/configs/{file}/original/raw":     "config.raw",
	"GET /api/v1/instances/{id}/configs/{file}/previous":         accessView,
	"GET /api/v1/instances/{id}/configs/{file}/previous/raw":     "config.raw",
	"GET /api/v1/instances/{id}/configs/{file}/raw":              "config.raw",
	"PUT /api/v1/instances/{id}/configs/{file}/raw":              "config.raw",
	"GET /api/v1/instances/{id}/diagnosis":                       accessView,
	"GET /api/v1/instances/{id}/disk":                            accessView,
	"GET /api/v1/instances/{id}/grants":                          accessHidden,
	"GET /api/v1/instances/{id}/grants/{user_id}":                accessHidden,
	"PUT /api/v1/instances/{id}/grants/{user_id}":                accessHidden,
	"DELETE /api/v1/instances/{id}/grants/{user_id}":             accessHidden,
	"GET /api/v1/instances/{id}/jobs":                            accessView,
	"GET /api/v1/instances/{id}/logs":                            accessView,
	"GET /api/v1/instances/{id}/manifest":                        "instance.settings",
	"GET /api/v1/instances/{id}/manifest/code":                   "instance.settings",
	"GET /api/v1/instances/{id}/mods":                            accessView,
	"POST /api/v1/instances/{id}/mods":                           "mods.manage",
	"GET /api/v1/instances/{id}/mods/export":                     accessView,
	"POST /api/v1/instances/{id}/mods/export/code":               accessView,
	"GET /api/v1/instances/{id}/mods/queue":                      accessView,
	"POST /api/v1/instances/{id}/mods/queue":                     "mods.manage",
	"DELETE /api/v1/instances/{id}/mods/queue/{full_name}":       "mods.manage",
	"POST /api/v1/instances/{id}/mods/resolve":                   "mods.manage",
	"POST /api/v1/instances/{id}/mods/code/apply":                "mods.manage",
	"POST /api/v1/instances/{id}/mods/code/preview":              "mods.manage",
	"POST /api/v1/instances/{id}/mods/updates":                   "mods.manage",
	"POST /api/v1/instances/{id}/mods/updates/queue":             "mods.manage",
	"POST /api/v1/instances/{id}/mods/updates/resolve":           "mods.manage",
	"DELETE /api/v1/instances/{id}/mods/{full_name}":             "mods.manage",
	"PATCH /api/v1/instances/{id}/mods/{full_name}":              "mods.manage",
	"GET /api/v1/instances/{id}/operation":                       accessView,
	"POST /api/v1/instances/{id}/operation/abandon":              accessAdmin,
	"POST /api/v1/instances/{id}/operation/resume":               accessAdmin,
	"GET /api/v1/instances/{id}/password":                        accessView,
	"GET /api/v1/instances/{id}/permitted":                       "players.manage",
	"PUT /api/v1/instances/{id}/permitted":                       "players.manage",
	"GET /api/v1/instances/{id}/players/history":                 accessView,
	"GET /api/v1/instances/{id}/players/seen":                    "players.manage",
	"GET /api/v1/instances/{id}/remote-copies":                   accessView,
	"GET /api/v1/instances/{id}/remote-copies/{copy_id}":         accessView,
	"POST /api/v1/instances/{id}/remote-copies/{copy_id}/cancel": "backups.create",
	"POST /api/v1/instances/{id}/remote-copies/{copy_id}/retry":  "backups.create",
	"POST /api/v1/instances/{id}/restart":                        "instance.restart",
	"GET /api/v1/instances/{id}/setups":                          accessAdmin,
	"POST /api/v1/instances/{id}/setups":                         accessAdmin,
	"GET /api/v1/instances/{id}/setups/{sid}":                    accessAdmin,
	"DELETE /api/v1/instances/{id}/setups/{sid}":                 accessAdmin,
	"GET /api/v1/instances/{id}/setups/{sid}/preview":            accessAdmin,
	"POST /api/v1/instances/{id}/setups/{sid}/restore":           accessAdmin,
	"POST /api/v1/instances/{id}/start":                          "instance.start",
	"GET /api/v1/instances/{id}/stats":                           accessView,
	"POST /api/v1/instances/{id}/stop":                           "instance.stop",
	"POST /api/v1/instances/{id}/update":                         accessAdmin,
	"GET /api/v1/instances/{id}/update-status":                   accessView,
	"GET /api/v1/instances/{id}/webhooks":                        "notifications.manage",
	"POST /api/v1/instances/{id}/webhooks":                       "notifications.manage",
	"GET /api/v1/instances/{id}/webhooks/deliveries":             "notifications.manage",
	"DELETE /api/v1/instances/{id}/webhooks/{wid}":               "notifications.manage",
	"PATCH /api/v1/instances/{id}/webhooks/{wid}":                "notifications.manage",
	"POST /api/v1/instances/{id}/webhooks/{wid}/test":            "notifications.manage",
	"GET /api/v1/instances/{id}/worlds":                          accessView,
	"POST /api/v1/instances/{id}/worlds/import":                  "world.import",
	"DELETE /api/v1/instances/{id}/worlds/{name}":                "world.import",
	"POST /api/v1/instances/{id}/worlds/{name}/restore":          "world.import",
}

// globalRouteAccess is who may call each route outside /api/v1/instances/{id}, for the same
// empty request.
var globalRouteAccess = map[string]string{
	"GET /api/v1/admin/alert-rules":                       accessHidden,
	"POST /api/v1/admin/alert-rules":                      accessHidden,
	"POST /api/v1/admin/alert-rules/test":                 accessHidden,
	"DELETE /api/v1/admin/alert-rules/{rid}":              accessHidden,
	"PATCH /api/v1/admin/alert-rules/{rid}":               accessHidden,
	"GET /api/v1/admin/diagnostics":                       accessAdmin,
	"GET /api/v1/admin/diagnostics/bundle":                accessAdmin,
	"POST /api/v1/admin/diagnostics/run":                  accessAdmin,
	"DELETE /api/v1/admin/discord":                        accessHidden,
	"GET /api/v1/admin/discord":                           accessHidden,
	"PUT /api/v1/admin/discord":                           accessHidden,
	"POST /api/v1/admin/keys/rotate":                      accessHidden,
	"POST /api/v1/admin/rclone-setups":                    accessHidden,
	"DELETE /api/v1/admin/rclone-setups/{id}":             accessHidden,
	"GET /api/v1/admin/rclone-setups/{id}":                accessHidden,
	"POST /api/v1/admin/rclone-setups/{id}/advance":       accessHidden,
	"POST /api/v1/admin/rclone-setups/{id}/finish":        accessHidden,
	"GET /api/v1/admin/remote-backup-destination":         accessHidden,
	"PUT /api/v1/admin/remote-backup-destination":         accessHidden,
	"GET /api/v1/admin/remote-backup-destination/remotes": accessHidden,
	"POST /api/v1/admin/remote-backup-destination/test":   accessHidden,
	"GET /api/v1/admin/shutdowns":                         accessHidden,
	"POST /api/v1/admin/shutdowns":                        accessHidden,
	"DELETE /api/v1/admin/shutdowns/{id}":                 accessHidden,
	"GET /api/v1/admin/webhooks":                          accessHidden,
	"POST /api/v1/admin/webhooks":                         accessHidden,
	"GET /api/v1/admin/webhooks/deliveries":               accessHidden,
	"DELETE /api/v1/admin/webhooks/{wid}":                 accessHidden,
	"PATCH /api/v1/admin/webhooks/{wid}":                  accessHidden,
	"POST /api/v1/admin/webhooks/{wid}/test":              accessHidden,
	"GET /api/v1/audit":                                   accessHidden,
	"GET /api/v1/audit/export":                            accessHidden,
	"GET /api/v1/audit/filters":                           accessHidden,
	"POST /api/v1/auth/login":                             accessPublic,
	"POST /api/v1/auth/logout":                            accessPublic,
	"GET /api/v1/auth/me":                                 accessMember,
	"GET /api/v1/game/options":                            accessMember,
	"GET /api/v1/instances":                               accessMember,
	"POST /api/v1/instances":                              accessAdmin,
	"POST /api/v1/instances/import":                       accessAdmin,
	"GET /api/v1/instances/inbox":                         accessMember,
	"POST /api/v1/instances/manifest/preview":             accessAdmin,
	"GET /api/v1/instances/orphans":                       accessAdmin,
	"GET /api/v1/invites":                                 accessHidden,
	"POST /api/v1/invites":                                accessHidden,
	"DELETE /api/v1/invites/{id}":                         accessHidden,
	"POST /api/v1/invites/{token}/redeem":                 accessPublic,
	"GET /api/v1/jobs/{id}":                               accessMember,
	"POST /api/v1/jobs/{id}/cancel":                       accessMember,
	"PATCH /api/v1/me":                                    accessMember,
	"POST /api/v1/me/password":                            accessMember,
	"GET /api/v1/me/permissions":                          accessMember,
	"GET /api/v1/mods/search":                             accessMember,
	"POST /api/v1/mods/sync":                              accessHidden,
	"GET /api/v1/mods/{namespace}/{name}":                 accessMember,
	"GET /api/v1/orphans/{container_id}":                  accessAdmin,
	"POST /api/v1/orphans/{container_id}":                 accessAdmin,
	"GET /api/v1/schedules":                               accessMember,
	"POST /api/v1/schedules":                              accessMember,
	"DELETE /api/v1/schedules/{id}":                       accessMember,
	"PATCH /api/v1/schedules/{id}":                        accessMember,
	"POST /api/v1/setup":                                  accessPublic,
	"GET /api/v1/templates":                               accessAdmin,
	"GET /api/v1/users":                                   accessHidden,
	"POST /api/v1/users":                                  accessHidden,
	"DELETE /api/v1/users/{id}":                           accessHidden,
	"PATCH /api/v1/users/{id}":                            accessHidden,
	"POST /api/v1/users/{id}/password/reset":              accessHidden,
	"GET /api/v1/ws":                                      accessSkip, // a WebSocket upgrade; ws tests cover it
}

const instanceRoutePrefix = "/api/v1/instances/{id}"

// grantableActions is every action a grant may carry: the operator set and the extras.
const grantableActions = `"instance.start","instance.stop","instance.restart","backups.create",` +
	`"backups.download","players.manage","commands.send","mods.manage","config.edit","config.raw",` +
	`"backups.restore","world.import","instance.settings","notifications.manage"`

// neverGrantableActions is the 09 §3.3 set; a grant naming any of them is ignored.
const neverGrantableActions = `"instance.create","instance.delete","instance.clone","instance.adopt",` +
	`"instance.limits","instance.extra_args","instance.image","instance.update","setups.manage",` +
	`"world.tools","users.manage","invites.manage","grants.manage","schedules.global",` +
	`"panel.settings","audit.read"`

// TestRouteAccessTablesCoverEveryRoute asserts every registered route is classified exactly
// once, so a new route cannot arrive without deciding who may call it.
func TestRouteAccessTablesCoverEveryRoute(t *testing.T) {
	t.Parallel()
	srv := router(t)
	seen := map[string]bool{}
	for _, route := range srv.routes {
		seen[route.pattern] = true
		_, inInstance := instanceRouteAccess[route.pattern]
		_, inGlobal := globalRouteAccess[route.pattern]
		_, path, _ := strings.Cut(route.pattern, " ")
		switch {
		case strings.HasPrefix(path, instanceRoutePrefix) && !inInstance:
			t.Errorf("instance route %s is missing from instanceRouteAccess", route.pattern)
		case !strings.HasPrefix(path, instanceRoutePrefix) && !inGlobal:
			t.Errorf("route %s is missing from globalRouteAccess", route.pattern)
		}
	}
	for _, table := range []map[string]string{instanceRouteAccess, globalRouteAccess} {
		for pattern := range table {
			if !seen[pattern] {
				t.Errorf("%s is classified but not registered", pattern)
			}
		}
	}
}

// TestEveryInstanceRouteAuthorizesItsInstance walks every instance route (D2, ADR-037,
// ADR-038). An instance the caller holds nothing on answers exactly as one that does not
// exist, since a 403 would confirm it is there and iterating ids would map every world on the
// panel. A viewer reaches the viewer routes and is refused the rest; each listed action opens
// its route on its own; and no grant, however wide, opens an admin route.
func TestEveryInstanceRouteAuthorizesItsInstance(t *testing.T) {
	t.Parallel()
	for pattern, access := range instanceRouteAccess {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, _ := strings.Cut(pattern, " ")
			target := func(id string) string {
				return wildcardPattern.ReplaceAllString(strings.ReplaceAll(path, "{id}", id), "sample")
			}
			plain := !strings.Contains(strings.TrimPrefix(path, instanceRoutePrefix), "{")

			rt, _, admin, member := world(t)
			invisible := as(rt, member, routeAccessRequest(method, target("inst-b")))
			missing := as(rt, member, routeAccessRequest(method, target("inst-nope")))
			if invisible.Code != http.StatusNotFound || errCode(t, invisible) != "not_found" {
				t.Errorf("invisible instance = %d %s, want 404 not_found", invisible.Code, invisible.Body)
			}
			a, b := errorBody(t, invisible), errorBody(t, missing)
			if invisible.Code != missing.Code || !reflect.DeepEqual(a, b) {
				t.Errorf("invisible = %d %v, nonexistent = %d %v; the two must not be distinguishable",
					invisible.Code, a, missing.Code, b)
			}

			viewer := as(rt, member, routeAccessRequest(method, target("inst-a")))
			switch access {
			case accessView:
				if viewer.Code == http.StatusForbidden || (plain && viewer.Code >= 300) {
					t.Errorf("viewer = %d %s, want it let through", viewer.Code, viewer.Body)
				}
			case accessHidden:
				wantRefused(t, "viewer", viewer, http.StatusNotFound, "not_found")
			default:
				wantRefused(t, "viewer", viewer, http.StatusForbidden, "forbidden")
			}
			if access == accessView && plain && method == http.MethodGet {
				if rec := as(rt, admin, routeAccessRequest(method, target("inst-a"))); rec.Code >= 300 {
					t.Errorf("admin = %d %s, want 2xx", rec.Code, rec.Body)
				}
			}

			every := "[" + grantableActions + "," + neverGrantableActions + "]"
			widest := grantedAs(t, "operator", every, method, target("inst-a"))
			switch access {
			case accessAdmin:
				wantRefused(t, "operator with every grant", widest, http.StatusForbidden, "forbidden")
			case accessHidden:
				wantRefused(t, "operator with every grant", widest, http.StatusNotFound, "not_found")
			default:
				if widest.Code == http.StatusForbidden {
					t.Errorf("operator with every grant = 403 %s, want it let through", widest.Body)
				}
			}

			if access == accessView || access == accessAdmin || access == accessHidden {
				return
			}
			for action := range strings.FieldsSeq(access) {
				rec := grantedAs(t, "viewer", `["`+action+`"]`, method, target("inst-a"))
				if rec.Code == http.StatusForbidden {
					t.Errorf("viewer granted %s = 403 %s, want it let through", action, rec.Body)
				}
			}
		})
	}
}

// TestEveryGlobalRouteAdmitsItsCallers asserts a member is refused every admin route, with
// 404 where the route is hidden from them and 403 where it is not, reaches every member route,
// and that an admin reads every admin listing.
func TestEveryGlobalRouteAdmitsItsCallers(t *testing.T) {
	t.Parallel()
	for pattern, access := range globalRouteAccess {
		if access == accessSkip {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()
			method, path, _ := strings.Cut(pattern, " ")
			plain := !strings.Contains(path, "{")
			target := wildcardPattern.ReplaceAllString(path, "sample")

			rt, _, admin, member := world(t)
			// An rclone that lists no remotes, so the admin listing does not depend on the host.
			rt.remoteBackups.Cfg.RemoteBackups.RcloneBinary = "true"
			rec := as(rt, member, routeAccessRequest(method, target))
			switch access {
			case accessAdmin:
				wantRefused(t, "member", rec, http.StatusForbidden, "forbidden")
			case accessHidden:
				wantRefused(t, "member", rec, http.StatusNotFound, "not_found")
			default:
				if rec.Code == http.StatusForbidden || (plain && rec.Code == http.StatusNotFound) {
					t.Errorf("member = %d %s, want it let through", rec.Code, rec.Body)
				}
			}
			if (access == accessAdmin || access == accessHidden) && plain && method == http.MethodGet {
				if rec := as(rt, admin, routeAccessRequest(method, target)); rec.Code >= 300 {
					t.Errorf("admin = %d %s, want 2xx", rec.Code, rec.Body)
				}
			}
		})
	}
}

// routeAccessRequest is the request the route tables describe: no query, and an empty JSON
// object as the body of anything that may carry one.
func routeAccessRequest(method, target string) *http.Request {
	body := ""
	if method != http.MethodGet && method != http.MethodDelete {
		body = "{}"
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// grantedAs sends the request as a member whose grant on inst-a has the given role and perms,
// in a world of its own, since a request that is let through may change it.
func grantedAs(t *testing.T, role, perms, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rt, db, _, member := world(t)
	seed(t, db, `UPDATE instance_grants SET role = ?, perms = ? WHERE user_id = 'u-member'`, role, perms)
	return as(rt, member, routeAccessRequest(method, target))
}

func wantRefused(t *testing.T, who string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || errCode(t, rec) != code {
		t.Errorf("%s = %d %s, want %d %s", who, rec.Code, rec.Body, status, code)
	}
}

// errorBody is the error envelope without its request id, which differs per request.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	delete(env.Error, "request_id")
	return env.Error
}
