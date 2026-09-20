package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

func openCondition(t *testing.T, db *store.DB, kind string, instanceID any) {
	t.Helper()
	seed(t, db, `INSERT INTO alert_conditions (id, instance_id, kind, detail, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, '{}', ?, ?)`, store.NewID(), instanceID, kind, store.Now(), store.Now())
}

func inboxItems(t *testing.T, rt *Router, u *store.User) []inboxItem {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inbox", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []inboxItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	return page.Items
}

// TestInboxIsScopedToVisibleInstances asserts no trace of an instance the caller cannot see
// reaches the response — not merely fewer rows (D2, ADR-038).
func TestInboxIsScopedToVisibleInstances(t *testing.T) {
	rt, db, ada, mel := world(t)
	openCondition(t, db, "restart_required", "inst-a")
	openCondition(t, db, "restart_required", "inst-b")

	if got := len(inboxItems(t, rt, ada)); got != 2 {
		t.Errorf("admin sees %d items, want both", got)
	}

	rec := as(rt, mel, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inbox", http.NoBody))
	if strings.Contains(rec.Body.String(), "inst-b") {
		t.Fatalf("a member granted only inst-a saw inst-b: %s", rec.Body.String())
	}
	items := inboxItems(t, rt, mel)
	if len(items) != 1 || items[0].InstanceID == nil || *items[0].InstanceID != "inst-a" {
		t.Errorf("member items = %+v, want only inst-a", items)
	}
}

// TestInboxHidesPanelOwnedConditionsFromMembers asserts a failed global job needs the global
// action, while low disk reaches anyone who can see an instance at all.
func TestInboxHidesPanelOwnedConditionsFromMembers(t *testing.T) {
	rt, db, ada, mel := world(t)
	openCondition(t, db, "job_failed", nil)
	openCondition(t, db, "low_disk", nil)

	if got := len(inboxItems(t, rt, ada)); got != 2 {
		t.Errorf("admin sees %d items, want both host-level conditions", got)
	}

	items := inboxItems(t, rt, mel)
	if len(items) != 1 || items[0].Kind != "low_disk" {
		t.Errorf("member items = %+v, want low_disk only", items)
	}
}

// TestRevokingAGrantEmptiesTheInbox asserts visibility is the whole gate for an instance-scoped
// condition: a member with no grant sees nothing, which is 09 §1's empty dashboard.
func TestRevokingAGrantEmptiesTheInbox(t *testing.T) {
	rt, db, _, mel := world(t)
	openCondition(t, db, "stale_backup", "inst-a")

	if got := len(inboxItems(t, rt, mel)); got != 1 {
		t.Fatalf("a granted instance's condition wants showing, got %d", got)
	}

	seed(t, db, `DELETE FROM instance_grants WHERE user_id = 'u-member'`)
	if got := len(inboxItems(t, rt, mel)); got != 0 {
		t.Errorf("an ungranted member must see nothing, got %d", got)
	}
}

// TestInboxSeverityMarksWorldDataRisks asserts the conditions that risk a world are separated
// from the ones that merely need attention.
func TestInboxSeverityMarksWorldDataRisks(t *testing.T) {
	rt, db, ada, _ := world(t)
	openCondition(t, db, "unclean_stop", "inst-a")
	openCondition(t, db, "update_available", "inst-b")

	got := map[string]string{}
	for _, item := range inboxItems(t, rt, ada) {
		got[item.Kind] = item.Severity
	}
	if got["unclean_stop"] != "critical" || got["update_available"] != "warning" {
		t.Errorf("severities = %v, want unclean_stop critical and update_available warning", got)
	}
}

// TestInboxIsAnUncursoredCollection asserts the envelope of 11 §1.
func TestInboxIsAnUncursoredCollection(t *testing.T) {
	rt, _, ada, _ := world(t)

	rec := as(rt, ada, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inbox", http.NoBody))
	var page struct {
		Items      []inboxItem `json:"items"`
		NextCursor *string     `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if page.Items == nil {
		t.Error("items must be an empty array, never null")
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %v, want null", *page.NextCursor)
	}
}
