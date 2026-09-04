package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// grantOperatorWith gives u-member the operator role on inst-a plus the named extras.
func grantOperatorWith(t *testing.T, db *store.DB, perms ...string) {
	t.Helper()
	raw, err := json.Marshal(perms)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instance_grants SET role = 'operator', perms = ?
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`, string(raw))
}

func patchInstance(t *testing.T, rt *Router, u *store.User, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a", jsonBody(t, body)))
}

// TestPatchSettingsRequiresTheGrantedAction is 09 §3.2: operator alone runs the server, it
// does not rename it. The extra has to be granted, and granting it must actually work.
func TestPatchSettingsRequiresTheGrantedAction(t *testing.T) {
	rt, db, _, member := world(t)
	grantOperatorWith(t, db)

	if rec := patchInstance(t, rt, member, map[string]any{"server_name": "Renamed"}); rec.Code != http.StatusForbidden {
		t.Errorf("without the extra = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	grantOperatorWith(t, db, "instance.settings")
	rec := patchInstance(t, rt, member, map[string]any{"server_name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("with the extra = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var updated store.Instance
	decodeInto(t, rec, &updated)
	if updated.ServerName != "Renamed" {
		t.Errorf("server_name = %q, want Renamed", updated.ServerName)
	}
	if !updated.RestartRequired {
		t.Error("restart_required not set; the running server still has the old name")
	}
}

// TestSettingsExtraDoesNotWidenTheAdminOnlyFields asserts the new grant reaches only the
// fields it names: limits and extra_args shape the container and stay admin-only (D15).
func TestSettingsExtraDoesNotWidenTheAdminOnlyFields(t *testing.T) {
	rt, db, _, member := world(t)
	grantOperatorWith(t, db, "instance.settings")

	for _, body := range []map[string]any{
		{"mem_limit_mb": 8192},
		{"cpu_limit": 2.0},
		{"extra_args": "-crossplay"},
		{"server_name": "Renamed", "mem_limit_mb": 8192},
	} {
		if rec := patchInstance(t, rt, member, body); rec.Code != http.StatusForbidden {
			t.Errorf("patch %v = %d, want 403", body, rec.Code)
		}
	}
}

// TestInstanceSettingsAppearsInAllowedActions asserts the UI can render the screen from
// allowed_actions rather than from a role name (F3, 09 §4.2).
func TestInstanceSettingsAppearsInAllowedActions(t *testing.T) {
	rt, db, _, member := world(t)

	grantOperatorWith(t, db)
	if slices.Contains(allowedActions(t, rt, member), "instance.settings") {
		t.Error("instance.settings is offered without the grant")
	}

	grantOperatorWith(t, db, "instance.settings")
	if !slices.Contains(allowedActions(t, rt, member), "instance.settings") {
		t.Error("instance.settings is granted but absent from allowed_actions, so no UI can show it")
	}
}

func allowedActions(t *testing.T, rt *Router, u *store.User) []string {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/me/permissions", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions = %d (%s)", rec.Code, rec.Body)
	}
	var body struct {
		Instances []struct {
			InstanceID     string   `json:"instance_id"`
			AllowedActions []string `json:"allowed_actions"`
		} `json:"instances"`
	}
	decodeInto(t, rec, &body)
	for _, inst := range body.Instances {
		if inst.InstanceID == "inst-a" {
			return inst.AllowedActions
		}
	}
	t.Fatal("inst-a is absent from the permissions payload")
	return nil
}

// TestPatchRejectsWorldName asserts world_name stays out: renaming a world moves its .db and
// .fwl pair (03 §4.1), so it is a file operation, not a column write. ADR-050 makes an
// unknown field a 400 rather than a silently dropped one, so the caller learns the edit did
// not happen (Q48).
func TestPatchRejectsWorldName(t *testing.T) {
	rt, db, admin, _ := world(t)

	rec := patchInstance(t, rt, admin, map[string]any{"world_name": "Elsewhere"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (ADR-050) (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "malformed_json") {
		t.Errorf("code is not malformed_json: %s", rec.Body)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.WorldName != "Worldinst-a" || inst.RestartRequired {
		t.Error("the rejected world_name still changed the row")
	}
}

// TestPatchEnforcesTheLaunchRules is 03 §1.3, checked against the merged row rather than the
// body: renaming a server can break a password the caller never mentioned.
func TestPatchEnforcesTheLaunchRules(t *testing.T) {
	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"password below the floor", map[string]any{"password": "abc"}, "password"},
		{"password inside the new server name", map[string]any{"server_name": "x pw-inst-a-secret x"}, "password"},
		{"a new password inside the existing world name", map[string]any{"password": "Worldinst-a"}, "password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, _ := world(t)
			rec := patchInstance(t, rt, admin, tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tt.field) {
				t.Errorf("the rejection does not name %s: %s", tt.field, rec.Body)
			}
			inst, err := db.InstanceByID(t.Context(), "inst-a")
			if err != nil {
				t.Fatal(err)
			}
			if inst.RestartRequired {
				t.Error("a rejected patch still marked the instance as changed")
			}
		})
	}
}

// TestTheHandlerAndContainerCreationAgree is G2: the same rule rejects the same value at both
// boundaries. A value the handler accepts but BuildSpec refuses is a server that provisions
// and never boots.
func TestTheHandlerAndContainerCreationAgree(t *testing.T) {
	rt, _, admin, _ := world(t)

	const shortPassword = "abc"
	rec := patchInstance(t, rt, admin, map[string]any{"password": shortPassword})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("the handler accepted %q: %d", shortPassword, rec.Code)
	}
	if v := instance.ValidateLaunch("Server inst-a", "Worldinst-a", shortPassword); len(v) == 0 {
		t.Error("container creation accepts a password the handler refused (G2)")
	}
}

// TestPatchCrossplayReachesTheContainerAndKeepsTheInstanceID is Q40's own case: a server
// created without crossplay can be given it. -instanceid is immutable for the instance's
// life (A5, ADR-027), so the rebuild must carry the old one through unchanged.
func TestPatchCrossplayReachesTheContainerAndKeepsTheInstanceID(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"crossplay": true, "public": true})))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	startAndWait(t, rt, admin)

	argv := currentContainer(t, db, fake).Spec.Cmd
	if !slices.Contains(argv, "-crossplay") {
		t.Errorf("argv = %v, want -crossplay after the toggle", argv)
	}
	if !containsArgValue(argv, "-public", "1") {
		t.Errorf("argv = %v, want -public 1", argv)
	}
	if !containsArgValue(argv, "-instanceid", "cp-inst-a") {
		t.Errorf("argv = %v, want the original -instanceid preserved (A5)", argv)
	}
}

// TestPatchPasswordReachesTheContainer asserts an edited game password is what the server is
// launched with, which is only true because the start rebuilds a drifted container.
func TestPatchPasswordReachesTheContainer(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	const next = "a-brand-new-password"
	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]any{"password": next})))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	startAndWait(t, rt, admin)

	if !containsArgValue(currentContainer(t, db, fake).Spec.Cmd, "-password", next) {
		t.Error("the new password did not reach the running container")
	}
}
