package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestPatchWatchdogValues asserts watchdog accepts off, alert and restart, refuses anything
// else with 422, and leaves the row unchanged after a refusal.
func TestPatchWatchdogValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		ok    bool
	}{
		{"off", "off", true},
		{"alert", "alert", true},
		{"restart", "restart", true},
		{"empty", "", false},
		{"upper case", "ALERT", false},
		{"unknown", "reboot", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, _ := world(t)
			rec := patchInstance(t, rt, admin, map[string]any{"watchdog": tt.value})
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
			wantStored := "off"
			if tt.ok {
				wantStored = tt.value
			}
			if inst.Watchdog != wantStored {
				t.Errorf("watchdog = %q, want %q", inst.Watchdog, wantStored)
			}
		})
	}
}

// TestPatchWatchdogDoesNotRequireARestart asserts the watchdog is not a launch setting: it shapes
// no container, so changing it must not tell the operator a restart is owed.
func TestPatchWatchdogDoesNotRequireARestart(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	if rec := patchInstance(t, rt, admin, map[string]any{"watchdog": "restart"}); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if inst.RestartRequired {
		t.Error("changing the watchdog set restart_required")
	}
}

// TestPatchWatchdogIsAudited asserts a change is recorded with its old and new value, and a
// value the row already holds records nothing.
func TestPatchWatchdogIsAudited(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)

	if rec := patchInstance(t, rt, admin, map[string]any{"watchdog": "off"}); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rows := auditRecordsFor(t, db, "instances.settings.update"); len(rows) != 0 {
		t.Fatalf("audit rows = %d for an unchanged value, want none", len(rows))
	}
	if rec := patchInstance(t, rt, admin, map[string]any{"watchdog": "alert"}); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rows := auditRecordsFor(t, db, "instances.settings.update")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	want := `{"changes":[{"field":"watchdog","from":"off","to":"alert"}]}`
	if got := deref(rows[0].Detail); got != want {
		t.Errorf("detail = %s\nwant     %s", got, want)
	}
}

// TestWatchdogRestartNeedsTheRightToRestart asserts alerting needs instance.settings alone and
// restarting a hung server also needs instance.restart.
func TestWatchdogRestartNeedsTheRightToRestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		perms []string
		value string
		want  int
	}{
		{"alert with settings", []string{"instance.settings"}, "alert", http.StatusOK},
		{"restart with settings alone", []string{"instance.settings"}, "restart", http.StatusForbidden},
		{
			"restart with settings and restart",
			[]string{"instance.settings", "instance.restart"},
			"restart", http.StatusOK,
		},
		{"alert with restart alone", []string{"instance.restart"}, "alert", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, _, member := world(t)
			raw, err := json.Marshal(tt.perms)
			if err != nil {
				t.Fatal(err)
			}
			seed(t, db, `UPDATE instance_grants SET role = 'viewer', perms = ?
				WHERE user_id = 'u-member' AND instance_id = 'inst-a'`, string(raw))
			rec := patchInstance(t, rt, member, map[string]any{"watchdog": tt.value})
			if rec.Code != tt.want {
				t.Fatalf("patch = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}
