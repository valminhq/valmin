package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestPatchSetupBeforeUpdateValues asserts setup_before_update accepts full, light and off,
// refuses anything else with 422, leaves the row unchanged after a refusal, and audits a change.
func TestPatchSetupBeforeUpdateValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, value string
		ok          bool
	}{
		{"full", "full", true},
		{"light", "light", true},
		{"off", "off", true},
		{"empty", "", false},
		{"upper case", "FULL", false},
		{"unknown", "none", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, _ := world(t)
			rec := patchInstance(t, rt, admin, map[string]any{"setup_before_update": tt.value})
			want := http.StatusUnprocessableEntity
			if tt.ok {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body)
			}
			inst, err := db.InstanceByID(t.Context(), "inst-a")
			if err != nil {
				t.Fatal(err)
			}
			wantStored := "full"
			if tt.ok {
				wantStored = tt.value
			}
			if inst.SetupBeforeUpdate != wantStored || inst.RestartRequired {
				t.Errorf("setup_before_update = %q, restart_required = %v, want %q and false",
					inst.SetupBeforeUpdate, inst.RestartRequired, wantStored)
			}
			rows := auditRecordsFor(t, db, "instances.settings.update")
			wantRows := 0
			if tt.ok && tt.value != "full" {
				wantRows = 1
			}
			if len(rows) != wantRows {
				t.Fatalf("audit rows = %d, want %d", len(rows), wantRows)
			}
			if wantRows == 1 {
				want := `{"changes":[{"field":"setup_before_update","from":"full","to":"` + tt.value + `"}]}`
				if got := deref(rows[0].Detail); got != want {
					t.Errorf("detail = %s\nwant     %s", got, want)
				}
			}
		})
	}
}

// TestSetupBeforeUpdateNeedsSetupsManage asserts a member who may change the server's settings
// still may not change which setups the panel saves.
func TestSetupBeforeUpdateNeedsSetupsManage(t *testing.T) {
	t.Parallel()
	rt, db, _, member := world(t)
	raw, err := json.Marshal([]string{"instance.settings", "mods.manage"})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instance_grants SET role = 'operator', perms = ?
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`, string(raw))
	rec := patchInstance(t, rt, member, map[string]any{"setup_before_update": "off"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}
