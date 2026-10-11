package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// TestEventAlertsReachOnlyTheirRulesDestinations asserts server-started and power cut notifications
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
	seedRule(t, db, alerts.KindServerStarted, "", ruled)
	seedRule(t, db, alerts.KindPowerCut, "", ruled)
	seedRule(t, db, alerts.KindPowerCut, "", ruled)
	elsewhere := "inst-b"
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-b', 'inst-b', 'stopped', '/srv/inst-b', 2466, 'Server', 'World', 'env', 'cp-b', ?, ?)`,
		store.Now(), store.Now())
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: store.NewID(), ConditionKind: alerts.KindServerStarted.String(), Params: "{}", Enabled: true,
		InstanceID: &elsewhere, WebhookIDs: []string{other},
	}); err != nil {
		t.Fatal(err)
	}

	rt.notifier.NotifyServerStarted(t.Context(), "inst-a", 0)
	now := time.Now().UTC()
	rt.notifier.NotifyPowerCutSoon(t.Context(), now.Add(7*time.Minute), now.Add(5*time.Minute), []string{"inst-a"})

	assertDeliveries(t, db, "the ruled destination", ruled,
		map[string]int{"server_started": 1, "power_cut_soon": 1})
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

// TestServerStoppedNamesItsReasonAndHonoursTheRulesOptions asserts a stop message carries why
// the server stopped, leaves out a field the rule hides, and shows its time in the rule's zone.
func TestServerStoppedNamesItsReasonAndHonoursTheRulesOptions(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, auto_stop_minutes, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', 'stopped', '/srv/inst-a', 2456, 'Server', 'Midgard', 'env', 'cp-a', 15, ?, ?)`,
		store.Now(), store.Now())
	ruled := seedWebhook(t, db, "ruled")
	seedRule(t, db, alerts.KindServerStopped,
		`{"hidden_fields":["world"],"timezone":"Asia/Tokyo"}`, ruled)

	rt.notifier.NotifyServerStopped(t.Context(), "inst-a", instance.StopNoPlayers, "", 0)

	sent := deliveriesOfKind(t, db, notify.KindServerStopped)
	if len(sent) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(sent))
	}
	var body struct {
		Detail map[string]string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(sent[0].Payload), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Detail["Reason"]; got != "No players for 15 minutes" {
		t.Errorf("reason = %q", got)
	}
	if _, ok := body.Detail["World"]; ok {
		t.Error("a hidden field was sent")
	}
	if got := body.Detail["Stopped"]; len(got) < 4 || got[len(got)-4:] != " JST" {
		t.Errorf("stopped at %q, want a time in JST", got)
	}
}
