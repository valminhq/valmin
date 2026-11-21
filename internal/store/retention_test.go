package store

import (
	"strings"
	"testing"
	"time"
)

// TestSweepExpiredKeepsWhatIsStillInUse asserts the sweep drops dead sessions, old resolved
// conditions and old terminal deliveries, and keeps the opening delivery of a condition that is
// still open, a pending delivery, and a live session.
func TestSweepExpiredKeepsWhatIsStillInUse(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	instanceID := seedInstance(t, db, NewID(), 2481)
	userID := seedUser(t, db, NewID())

	session := func(idleExpires time.Time, revoked bool) string {
		id := NewID()
		var revokedAt any
		if revoked {
			revokedAt = FormatTime(now)
		}
		exec(t, db.Writer, `INSERT INTO sessions (id, token_hash, user_id, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, id, userID, FormatTime(old), FormatTime(old), FormatTime(idleExpires),
			FormatTime(now.Add(time.Hour)), revokedAt)
		return id
	}
	live := session(now.Add(time.Hour), false)
	session(now.Add(-time.Minute), false)
	session(now.Add(time.Hour), true)

	stillOpen := observed("low_disk", &instanceID)
	resolved := observed("backup_overdue", &instanceID)
	opened, err := db.ReconcileConditions(ctx, []ObservedCondition{stillOpen, resolved}, old.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReconcileConditions(ctx, []ObservedCondition{stillOpen}, old); err != nil {
		t.Fatal(err)
	}
	webhookID := seedWebhook(t, db)
	rule := &AlertRule{ID: NewID(), ConditionKind: "low_disk", Params: "{}", Enabled: true}
	if err := db.SaveAlertRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	delivery := func(eventID string) *Delivery {
		return &Delivery{ID: NewID(), WebhookID: webhookID, EventID: eventID, EventKind: "alert", Payload: "{}"}
	}
	byCondition := map[string]*Delivery{}
	for _, c := range opened.Opened {
		d := delivery("event-" + c.ID)
		claim := &Notification{ConditionID: c.ID, RuleID: rule.ID, Edge: EdgeOpened, EventID: d.EventID}
		if _, err := db.MarkNotified(ctx, claim, old, []*Delivery{d}); err != nil {
			t.Fatal(err)
		}
		byCondition[c.Kind] = d
	}
	unrelated, pending := delivery("event-started"), delivery("event-stopped")
	for _, d := range []*Delivery{unrelated, pending} {
		if err := db.CreateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, db.Writer, `UPDATE webhook_deliveries SET status = 'delivered', updated_at = ? WHERE id <> ?`,
		FormatTime(old), pending.ID)
	exec(t, db.Writer, `UPDATE webhook_deliveries SET updated_at = ? WHERE id = ?`, FormatTime(old), pending.ID)

	if err := db.SweepExpired(ctx, now, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	ids := func(query string) []string {
		t.Helper()
		rows, err := db.Reader.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := ids(`SELECT id FROM sessions`); len(got) != 1 || got[0] != live {
		t.Errorf("sessions after the sweep = %v, want only the live %s", got, live)
	}
	if got := ids(`SELECT kind FROM alert_conditions`); len(got) != 1 || got[0] != "low_disk" {
		t.Errorf("conditions after the sweep = %v, want only the open low_disk", got)
	}
	got := strings.Join(ids(`SELECT id FROM webhook_deliveries ORDER BY id`), ",")
	want := []string{byCondition["low_disk"].ID, pending.ID}
	if !strings.Contains(got, want[0]) || !strings.Contains(got, want[1]) || strings.Count(got, ",") != 1 {
		t.Errorf("deliveries after the sweep = %s, want the open condition's %s and the pending %s",
			got, want[0], want[1])
	}
}

// TestDeliveryLookupsUseTheirIndexes asserts the pending poll and the lookup by event are index
// searches, not walks of the whole delivery history.
func TestDeliveryLookupsUseTheirIndexes(t *testing.T) {
	t.Parallel()
	db := open(t)
	for _, tc := range []struct {
		query string
		args  []any
		index string
	}{
		{
			`SELECT ` + deliveryColumns + ` FROM webhook_deliveries
			WHERE status = ? ORDER BY created_at, id LIMIT ?`,
			[]any{DeliveryPending, 10},
			"webhook_deliveries_pending",
		},
		{`SELECT ` + deliveryColumns + ` FROM webhook_deliveries WHERE event_id = ? AND webhook_id = ?
			ORDER BY created_at, id LIMIT 1`, []any{"e", "w"}, "webhook_deliveries_event"},
	} {
		if plan := queryPlan(t, db, tc.query, tc.args...); !strings.Contains(plan, tc.index) {
			t.Errorf("plan = %q, want it to use %s", plan, tc.index)
		}
	}
}

// queryPlan returns the EXPLAIN QUERY PLAN details of query, joined by "; ".
func queryPlan(t *testing.T, db *DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "; ")
}
