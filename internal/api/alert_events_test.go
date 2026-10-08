package api

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/store"
)

// TestEventAlertsReachOnlyTheirRulesDestinations asserts auto-stop and power cut notifications
// go to the destinations of a matching rule and to no other, once each even when two rules
// name the same destination, and that a rule for another server does not match.
func TestEventAlertsReachOnlyTheirRulesDestinations(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', 'running', '/srv/inst-a', 2456, 'Server', 'World', 'env', 'cp-a', ?, ?)`,
		store.Now(), store.Now())
	ruled := seedWebhook(t, db, "ruled")
	other := seedWebhook(t, db, "other")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindAutoStopped, "", ruled)
	seedRule(t, db, alerts.KindPowerCut, "", ruled)
	seedRule(t, db, alerts.KindPowerCut, "", ruled)
	elsewhere := "inst-b"
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-b', 'inst-b', 'stopped', '/srv/inst-b', 2466, 'Server', 'World', 'env', 'cp-b', ?, ?)`,
		store.Now(), store.Now())
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: store.NewID(), ConditionKind: alerts.KindAutoStopped.String(), Params: "{}", Enabled: true,
		InstanceID: &elsewhere, WebhookIDs: []string{other},
	}); err != nil {
		t.Fatal(err)
	}

	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	rt.notifier.NotifyAutoStopped(t.Context(), inst)
	now := time.Now().UTC()
	rt.notifier.NotifyPowerCutSoon(t.Context(), now.Add(7*time.Minute), now.Add(5*time.Minute), []string{"inst-a"})

	assertDeliveries(t, db, "the ruled destination", ruled,
		map[string]int{"instance_auto_stopped": 1, "power_cut_soon": 1})
	assertDeliveries(t, db, "a rule for another server", other, map[string]int{})
	assertDeliveries(t, db, "a destination no rule names", everyone, map[string]int{})
}

// TestAnEventAlertIsNotSentInQuietHours asserts a rule inside its quiet window sends nothing.
func TestAnEventAlertIsNotSentInQuietHours(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	ruled := seedWebhook(t, db, "ruled")
	start, end, zone := 0, 1439, "UTC"
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: store.NewID(), ConditionKind: alerts.KindPowerCut.String(), Params: "{}", Enabled: true,
		QuietStart: &start, QuietEnd: &end, QuietTZ: &zone, WebhookIDs: []string{ruled},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if m := now.Hour()*60 + now.Minute(); m == 1439 {
		t.Skip("the quiet window ends this minute")
	}
	rt.notifier.NotifyPowerCutSoon(t.Context(), now.Add(7*time.Minute), now.Add(5*time.Minute), nil)
	assertDeliveries(t, db, "a quiet rule's destination", ruled, map[string]int{})
}
