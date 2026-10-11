package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/store"
)

const alertRulesPath = "/api/v1/admin/alert-rules"

// ruleAudits returns the detail of every alert rule audit row, oldest first.
func ruleAudits(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Reader.QueryContext(t.Context(), `
		SELECT detail FROM audit_log
		WHERE action = 'panel.settings' AND detail LIKE '%alert_rule_%' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAlertRuleChangesAreAudited asserts that create, patch and delete each write one audit
// row naming the operation and the rule.
func TestAlertRuleChangesAreAudited(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	hook := createWebhook(t, rt, admin,
		`{"name":"Discord","kind":"discord","url":"https://example.com/api/webhooks/1/x"}`)

	req := httptest.NewRequest(http.MethodPost, alertRulesPath, strings.NewReader(
		`{"condition_kind":"low_disk","webhook_ids":["`+hook.ID+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := as(rt, admin, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var rule alertRuleView
	decodeInto(t, rec, &rule)

	for _, tc := range []struct {
		method, body string
		status       int
		operation    string
	}{
		{http.MethodPatch, `{"enabled":false}`, http.StatusOK, "alert_rule_update"},
		{http.MethodDelete, "", http.StatusNoContent, "alert_rule_delete"},
	} {
		req := httptest.NewRequest(tc.method, alertRulesPath+"/"+rule.ID, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		if rec := as(rt, admin, req); rec.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.method, rec.Code, rec.Body)
		}
	}

	got := ruleAudits(t, db)
	want := []string{
		`{"operation":"alert_rule_create","alert_rule_id":"` + rule.ID + `"}`,
		`{"operation":"alert_rule_update","alert_rule_id":"` + rule.ID + `"}`,
		`{"operation":"alert_rule_delete","alert_rule_id":"` + rule.ID + `"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit rows = %q, want %q", got, want)
	}
}

// TestDeletingAnUnknownAlertRuleIs404 asserts a delete that names no rule answers 404 and
// writes no audit row.
func TestDeletingAnUnknownAlertRuleIs404(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete, alertRulesPath+"/nope", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if got := ruleAudits(t, db); len(got) != 0 {
		t.Errorf("audit rows = %q, want none", got)
	}
}

// TestHostLevelRuleCannotNameAServer asserts a low disk rule scoped to one server is refused
// with 422 on instance_id, whether the request creates it or patches an existing rule into it.
func TestHostLevelRuleCannotNameAServer(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	seedTestInstance(t, db, "midgard", 24560)
	hook := createWebhook(t, rt, admin,
		`{"name":"Discord","kind":"discord","url":"https://example.com/api/webhooks/1/x"}`)

	req := httptest.NewRequest(http.MethodPost, alertRulesPath, strings.NewReader(
		`{"condition_kind":"crash_loop","instance_id":"midgard","webhook_ids":["`+hook.ID+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := as(rt, admin, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create scoped crash_loop: %d %s", rec.Code, rec.Body)
	}
	var scoped alertRuleView
	decodeInto(t, rec, &scoped)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{
			"create", http.MethodPost, alertRulesPath,
			`{"condition_kind":"low_disk","instance_id":"midgard","webhook_ids":["` + hook.ID + `"]}`,
		},
		{"patch", http.MethodPatch, alertRulesPath + "/" + scoped.ID, `{"condition_kind":"low_disk"}`},
		{
			"create a power cut rule", http.MethodPost, alertRulesPath,
			`{"condition_kind":"power_cut","instance_id":"midgard","webhook_ids":["` + hook.ID + `"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := as(rt, admin, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"instance_id"`) {
				t.Errorf("body does not name instance_id: %s", rec.Body)
			}
		})
	}
}

// TestAlertRuleThresholdsAreRangeChecked asserts a negative count or duration, a duration too
// long to hold, or a stale factor of 1 or less other than 0, is refused with 422 naming the
// field, on create and on patch, and that sent thresholds replace the stored ones wholesale.
func TestAlertRuleThresholdsAreRangeChecked(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)

	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return as(rt, admin, req)
	}
	rec := send(http.MethodPost, alertRulesPath,
		`{"condition_kind":"crash_loop","params":{"crash_count":5,"crash_window_seconds":600}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var existing alertRuleView
	decodeInto(t, rec, &existing)

	for _, tc := range []struct {
		params string
		field  string
	}{
		{`{"crash_count":-1}`, "params.crash_count"},
		{`{"crash_window_seconds":-60}`, "params.crash_window_seconds"},
		{`{"stuck_after_seconds":-1}`, "params.stuck_after_seconds"},
		{`{"stuck_after_seconds":10000000000}`, "params.stuck_after_seconds"},
		{`{"crash_window_seconds":20000000000}`, "params.crash_window_seconds"},
		{`{"stale_factor":1}`, "params.stale_factor"},
		{`{"stale_factor":0.5}`, "params.stale_factor"},
		{`{"stale_factor":-2}`, "params.stale_factor"},
		{`{"crash_count":0,"stale_factor":0}`, ""},
		{`{"stuck_after_seconds":5400,"stale_factor":1.5}`, ""},
	} {
		for _, target := range []struct{ method, path string }{
			{http.MethodPost, alertRulesPath},
			{http.MethodPatch, alertRulesPath + "/" + existing.ID},
		} {
			t.Run(target.method+" "+tc.params, func(t *testing.T) {
				rec := send(target.method, target.path,
					`{"condition_kind":"crash_loop","params":`+tc.params+`}`)
				if tc.field == "" {
					if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
						t.Fatalf("status = %d, want success (%s)", rec.Code, rec.Body)
					}
					return
				}
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body)
				}
				want := `"field":"` + tc.field + `","code":"out_of_range"`
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("body does not carry %s: %s", want, rec.Body)
				}
			})
		}
	}

	var got alertRuleView
	decodeInto(t, send(http.MethodPatch, alertRulesPath+"/"+existing.ID,
		`{"params":{"stuck_after_seconds":900}}`), &got)
	if !reflect.DeepEqual(got.Params, alerts.ParamsWire{StuckAfterSeconds: 900}) {
		t.Errorf("params = %+v, want only stuck_after_seconds 900", got.Params)
	}
}
