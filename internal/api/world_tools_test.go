package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// seedMod records pkg as installed on inst-a.
func seedMod(t *testing.T, db *store.DB, pkg string) {
	t.Helper()
	seed(t, db, `INSERT INTO instance_mods (
		instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at
	) VALUES ('inst-a', ?, '1.0.0', 'explicit', 'server_only', TRUE, '[]', ?)`, pkg, store.Now())
}

// TestRunWorldToolRefuses asserts every refusal the world tools endpoint answers before it
// queues a job.
func TestRunWorldToolRefuses(t *testing.T) {
	const worldClean = `{"tool":"upgrade_world","action":"world_clean"}`
	tests := []struct {
		name     string
		admin    bool
		instance string
		body     string
		setup    func(t *testing.T, db *store.DB)
		status   int
		code     string
	}{
		{"invisible instance", false, "inst-b", worldClean, nil, http.StatusNotFound, "not_found"},
		{"viewer", false, "inst-a", worldClean, nil, http.StatusForbidden, "forbidden"},
		{
			"malformed operation", true, "inst-a", `{"tool":"upgrade_world","action":"upgrade","operation":"A"}`,
			nil, http.StatusUnprocessableEntity, "validation_failed",
		},
		{
			"unknown action", true, "inst-a", `{"tool":"fresh_world","action":"world_clean"}`,
			nil, http.StatusUnprocessableEntity, "validation_failed",
		},
		{"running server", true, "inst-a", worldClean, func(t *testing.T, db *store.DB) {
			seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
		}, http.StatusConflict, "invalid_state"},
		{"tool not installed", true, "inst-a", worldClean, func(t *testing.T, db *store.DB) {
			seedMod(t, db, command.ValheimRCONPackage)
		}, http.StatusConflict, "unsupported"},
		{"rcon not installed", true, "inst-a", worldClean, func(t *testing.T, db *store.DB) {
			seedMod(t, db, control.UpgradeWorldPackage)
		}, http.StatusConflict, "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, member := world(t)
			if tt.setup != nil {
				tt.setup(t, db)
			}
			u := member
			if tt.admin {
				u = admin
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+tt.instance+"/world-tools",
				bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := as(rt, u, req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, tt.status)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, tt.code)
			}
		})
	}
}

// TestCapabilitiesReportInstalledWorldTools asserts world_tools follows instance_mods.
func TestCapabilitiesReportInstalledWorldTools(t *testing.T) {
	rt, db, admin, _ := world(t)
	seedMod(t, db, control.FreshWorldPackage)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-a/capabilities", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	var got struct {
		WorldTools map[string]bool `json:"world_tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.WorldTools["fresh_world"] || got.WorldTools["upgrade_world"] {
		t.Errorf("world_tools = %v, want only fresh_world", got.WorldTools)
	}
}
