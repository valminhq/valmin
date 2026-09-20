package store

import (
	"testing"
	"time"
)

func observed(kind string, instanceID *string) ObservedCondition {
	return ObservedCondition{Kind: kind, InstanceID: instanceID, Detail: map[string]string{"k": "v"}}
}

// TestReconcileOpensThenStaysQuiet asserts a condition still true on a later scan does not
// open again, and that only last_seen_at moves.
func TestReconcileOpensThenStaysQuiet(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	first, err := db.ReconcileConditions(t.Context(), []ObservedCondition{observed("low_disk", &id)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Opened) != 1 || len(first.Resolved) != 0 {
		t.Fatalf("first scan = %d opened, %d resolved, want 1 and 0", len(first.Opened), len(first.Resolved))
	}

	second, err := db.ReconcileConditions(t.Context(),
		[]ObservedCondition{observed("low_disk", &id)}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Opened) != 0 || len(second.Resolved) != 0 {
		t.Fatalf("second scan = %d opened, %d resolved, want 0 and 0", len(second.Opened), len(second.Resolved))
	}

	open, err := db.OpenConditions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conditions = %d, want the one row both scans saw", len(open))
	}
	if !open[0].LastSeenAt.After(open[0].FirstSeenAt) {
		t.Error("the second scan must advance last_seen_at without moving first_seen_at")
	}
}

// TestReconcileResolvesWhatIsNoLongerTrue asserts a condition absent from a later scan is
// resolved rather than deleted, and leaves the inbox.
func TestReconcileResolvesWhatIsNoLongerTrue(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	if _, err := db.ReconcileConditions(t.Context(),
		[]ObservedCondition{observed("restart_required", &id)}, now); err != nil {
		t.Fatal(err)
	}
	diff, err := db.ReconcileConditions(t.Context(), nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Resolved) != 1 || diff.Resolved[0].Kind != "restart_required" {
		t.Fatalf("resolved = %+v, want the one condition that stopped being true", diff.Resolved)
	}

	open, err := db.OpenConditions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("open conditions = %d, want none once it resolved", len(open))
	}
}

// TestReconcileReopensAfterResolving asserts the unique index admits a closed row beside a
// new open one, so a second occurrence is not lost.
func TestReconcileReopensAfterResolving(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	for i, obs := range [][]ObservedCondition{
		{observed("instance_error", &id)},
		nil,
		{observed("instance_error", &id)},
	} {
		if _, err := db.ReconcileConditions(t.Context(), obs, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}

	open, err := db.OpenConditions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open conditions = %d, want the reopened one", len(open))
	}
	if open[0].ResolvedAt != nil {
		t.Error("the reopened condition must be open")
	}
}

// TestReconcileKeepsHostAndInstanceConditionsApart asserts a null instance is its own key and
// matches itself across scans, which is what the index's COALESCE buys.
func TestReconcileKeepsHostAndInstanceConditionsApart(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	scan := []ObservedCondition{observed("low_disk", nil), observed("low_disk", &id)}
	first, err := db.ReconcileConditions(t.Context(), scan, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Opened) != 2 {
		t.Fatalf("opened = %d, want the host row and the instance row", len(first.Opened))
	}

	second, err := db.ReconcileConditions(t.Context(), scan, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Opened) != 0 {
		t.Errorf("opened again = %d, want 0 — the host row must match itself across scans", len(second.Opened))
	}
}

// TestReconcileIgnoresADuplicateObservation asserts a doubled observation opens one row.
func TestReconcileIgnoresADuplicateObservation(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	diff, err := db.ReconcileConditions(t.Context(),
		[]ObservedCondition{observed("job_failed", &id), observed("job_failed", &id)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Opened) != 1 {
		t.Fatalf("opened = %d, want 1 from a doubled observation", len(diff.Opened))
	}
}

// TestMarkNotifiedIsClaimedOnce asserts one claim per edge, and that the two edges are
// claimed separately.
func TestMarkNotifiedIsClaimedOnce(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	diff, err := db.ReconcileConditions(t.Context(), []ObservedCondition{observed("low_disk", &id)}, now)
	if err != nil {
		t.Fatal(err)
	}
	conditionID := diff.Opened[0].ID

	rule := &AlertRule{ID: NewID(), ConditionKind: "low_disk", Params: "{}", Enabled: true}
	if err := db.SaveAlertRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}

	claimed, err := db.MarkNotified(t.Context(), conditionID, rule.ID, EdgeOpened, now)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, err %v, want true", claimed, err)
	}
	again, err := db.MarkNotified(t.Context(), conditionID, rule.ID, EdgeOpened, now)
	if err != nil || again {
		t.Fatalf("second claim = %v, err %v, want false", again, err)
	}
	// The other edge is a separate claim: resolving is its own message.
	resolved, err := db.MarkNotified(t.Context(), conditionID, rule.ID, EdgeResolved, now)
	if err != nil || !resolved {
		t.Fatalf("resolve claim = %v, err %v, want true", resolved, err)
	}
}

// TestSaveAlertRuleReplacesDestinations asserts destinations are the set last written, not a
// union of every set.
func TestSaveAlertRuleReplacesDestinations(t *testing.T) {
	db := open(t)
	first, second := seedWebhook(t, db), seedWebhook(t, db)

	rule := &AlertRule{
		ID: NewID(), ConditionKind: "low_disk", Params: "{}", Enabled: true,
		WebhookIDs: []string{first, second},
	}
	if err := db.SaveAlertRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}
	rule.WebhookIDs = []string{second}
	if err := db.SaveAlertRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}

	got, err := db.AlertRuleByID(t.Context(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WebhookIDs) != 1 || got.WebhookIDs[0] != second {
		t.Errorf("destinations = %v, want only %s", got.WebhookIDs, second)
	}
}

// TestDeletingAWebhookRemovesItFromRules asserts the join table's cascade, so no rule keeps
// naming a destination that is gone.
func TestDeletingAWebhookRemovesItFromRules(t *testing.T) {
	db := open(t)
	webhookID := seedWebhook(t, db)
	rule := &AlertRule{
		ID: NewID(), ConditionKind: "low_disk", Params: "{}", Enabled: true,
		WebhookIDs: []string{webhookID},
	}
	if err := db.SaveAlertRule(t.Context(), rule); err != nil {
		t.Fatal(err)
	}
	exec(t, db.Writer, `DELETE FROM webhooks WHERE id = ?`, webhookID)

	got, err := db.AlertRuleByID(t.Context(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WebhookIDs) != 0 {
		t.Errorf("destinations = %v, want none after the webhook was deleted", got.WebhookIDs)
	}
}

// TestRecentIncidentsWindowsAndSweeps asserts the count is windowed and the sweep drops what
// is past retention.
func TestRecentIncidentsWindowsAndSweeps(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2471)
	now := time.Now().UTC()

	for _, age := range []time.Duration{time.Minute, 2 * time.Minute, 90 * time.Minute} {
		if err := db.RecordIncident(t.Context(), id, "exited", now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.RecentIncidents(t.Context(), now.Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 2 {
		t.Errorf("incidents inside the window = %d, want 2", len(got[id]))
	}

	if _, err := db.SweepIncidents(t.Context(), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, err := db.RecentIncidents(t.Context(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(after[id]) != 2 {
		t.Errorf("incidents after the sweep = %d, want the 2 inside retention", len(after[id]))
	}
}

func seedWebhook(t *testing.T, db *DB) string {
	t.Helper()
	id := NewID()
	exec(t, db.Writer, `
		INSERT INTO webhooks (id, name, kind, url, enabled, created_at, updated_at)
		VALUES (?, ?, 'generic', 'sealed', TRUE, ?, ?)`, id, "dest-"+id, Now(), Now())
	return id
}
