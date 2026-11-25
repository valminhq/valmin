package api

import (
	"net/http"
	"testing"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// seedRule routes one condition kind to the given destinations and returns the rule's id.
func seedRule(t *testing.T, db *store.DB, kind alerts.Kind, params string, webhookIDs ...string) string {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	id := store.NewID()
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: id, ConditionKind: kind.String(), Params: params, Enabled: true,
		WebhookIDs: webhookIDs,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// deliveriesTo reads every delivery row one destination was owed, by event kind.
func deliveriesTo(t *testing.T, db *store.DB, webhookID string) map[string]int {
	t.Helper()
	all, err := db.ListDeliveries(t.Context(), store.DeliveryFilter{}, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for i := range all {
		if all[i].WebhookID == webhookID {
			out[all[i].EventKind]++
		}
	}
	return out
}

func assertDeliveries(t *testing.T, db *store.DB, who, webhookID string, want map[string]int) {
	t.Helper()
	got := deliveriesTo(t, db, webhookID)
	if len(got) != len(want) {
		t.Errorf("%s was owed %v, want %v", who, got, want)
		return
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("%s was owed %v, want %v", who, got, want)
			return
		}
	}
}

// scanNow runs one alert scan the way the scheduled job does.
func scanNow(t *testing.T, rt *Server) {
	t.Helper()
	if _, err := rt.instances.scanAlerts(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// failJob records a terminal failed job and runs the finish hook the engine runs with it.
func failJob(t *testing.T, rt *Server, db *store.DB, kind jobs.Kind, instanceID string) {
	t.Helper()
	id := store.NewID()
	seed(t, db, `INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name,
		error_code, created_at, started_at, finished_at)
		VALUES (?, ?, 'failed', ?, ?, 'Midgard', 'internal', ?, ?, ?)`,
		id, kind.String(), jobs.InstanceLockKey(instanceID), instanceID,
		store.Now(), store.Now(), store.Now())
	finishJob(t, rt, db, &jobs.FinishedJob{
		ID: id, Kind: kind, InstanceID: &instanceID, InstanceName: "Midgard",
		Status: jobs.StatusFailed,
	})
}

// TestAFailedBackupReachesOnlyBackupFailedRuleDestinations asserts a failed backup is owed, in
// its finish transaction, to the destinations of the backup_failed rules and to nothing else: not
// a destination no rule names, nor one only a job_failed rule names, which hears at the next scan.
func TestAFailedBackupReachesOnlyBackupFailedRuleDestinations(t *testing.T) {
	t.Parallel()
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	jobRuled := seedWebhook(t, db, "job-ruled")
	everyone := seedWebhook(t, db, "everyone")
	ruleID := seedRule(t, db, alerts.KindBackupFailed, "", ruled)
	seedRule(t, db, alerts.KindJobFailed, "", jobRuled)
	instanceID := seedStoppedInstance(t, db, "backup-fail").ID

	failJob(t, rt, db, jobs.KindBackup, instanceID)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindBackupFailed.String(): 1})
	assertDeliveries(t, db, "a job_failed rule's destination", jobRuled, map[string]int{})
	assertDeliveries(t, db, "a destination no rule names", everyone, map[string]int{})
	sent, err := db.ListDeliveries(t.Context(), store.DeliveryFilter{RuleID: ruleID}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 {
		t.Errorf("the rule recorded %d deliveries, want 1", len(sent))
	}
}

// TestEveryFailedBackupIsSent asserts a backup_failed rule announces each failure, even while an
// earlier failure on the server is unresolved.
func TestEveryFailedBackupIsSent(t *testing.T) {
	t.Parallel()
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	seedRule(t, db, alerts.KindBackupFailed, "", ruled)
	instanceID := seedStoppedInstance(t, db, "fails-twice").ID

	failJob(t, rt, db, jobs.KindRestore, instanceID)
	failJob(t, rt, db, jobs.KindBackup, instanceID)
	scanNow(t, rt)
	failJob(t, rt, db, jobs.KindBackup, instanceID)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindBackupFailed.String(): 2})
}

// TestAQuietBackupFailedRuleSendsNothing asserts a failed backup inside the rule's quiet hours
// is not sent.
func TestAQuietBackupFailedRuleSendsNothing(t *testing.T) {
	t.Parallel()
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	rule := seedRule(t, db, alerts.KindBackupFailed, "", ruled)
	seed(t, db, `UPDATE alert_rules SET quiet_start = 0, quiet_end = 1439, quiet_tz = 'UTC'
		WHERE id = ?`, rule)
	instanceID := seedStoppedInstance(t, db, "quiet").ID

	failJob(t, rt, db, jobs.KindBackup, instanceID)

	assertDeliveries(t, db, "a quiet rule's destination", ruled, map[string]int{})
}

// TestANewBuildReachesOnlyNewGameBuildRuleDestinations asserts a public build the panel has not
// seen is owed once to each destination of the new_game_build rules, and an unchanged one, or
// one no rule wants, owes nothing.
func TestANewBuildReachesOnlyNewGameBuildRuleDestinations(t *testing.T) {
	t.Parallel()
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	if rt.notifier.NotifyPublicBuild(t.Context(), "21981590", "22000000") != nil {
		t.Error("a new build owes a notification with no rule for it")
	}
	ruled := seedWebhook(t, db, "ruled")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindNewGameBuild, "", ruled)

	if rt.notifier.NotifyPublicBuild(t.Context(), "22000000", "22000000") != nil {
		t.Error("an unchanged build owes a notification")
	}
	owed := rt.notifier.NotifyPublicBuild(t.Context(), "21981590", "22000000")
	if owed == nil {
		t.Fatal("a new public build owes no notification")
	}
	runInTx(t, db, owed)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindUpdateAvailable.String(): 1})
	assertDeliveries(t, db, "a destination no rule names", everyone, map[string]int{})
}

// TestAnUnexpectedStopReachesOnlyInstanceDownRuleDestinations asserts an unexpected stop is
// sent to the destinations of the instance_down rules, once each, and to nothing else.
func TestAnUnexpectedStopReachesOnlyInstanceDownRuleDestinations(t *testing.T) {
	t.Parallel()
	rt, db, fake := supervisorWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindInstanceDown, "", ruled)
	seedRule(t, db, alerts.KindInstanceDown, "", ruled)
	containerID := seedInstance(t, rt, db, fake, "running")

	fake.Get(containerID).Exit(0)
	if err := rt.supervisor.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	assertDeliveries(t, db, "the rules' destination", ruled,
		map[string]int{notify.KindInstanceDown.String(): 1})
	assertDeliveries(t, db, "a destination no rule names", everyone, map[string]int{})
}

// TestAServerRuleWinsOnASharedChannel asserts that when a rule about one server and a rule for
// every server both post to a shared channel, the channel gets one message, the server's.
func TestAServerRuleWinsOnASharedChannel(t *testing.T) {
	t.Parallel()
	rt, db, fake := supervisorWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	shared := seedWebhook(t, db, "shared")
	containerID := seedInstance(t, rt, db, fake, "running")
	seedRule(t, db, alerts.KindInstanceDown, "", shared)
	instanceID := seededInstanceID
	own := store.NewID()
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: own, ConditionKind: alerts.KindInstanceDown.String(), Params: "{}", Enabled: true,
		InstanceID: &instanceID, WebhookIDs: []string{shared},
	}); err != nil {
		t.Fatal(err)
	}

	fake.Get(containerID).Exit(0)
	if err := rt.supervisor.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}

	sent, err := db.ListDeliveries(t.Context(), store.DeliveryFilter{WebhookID: shared}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].RuleID == nil || *sent[0].RuleID != own {
		t.Errorf("the shared channel was sent %+v, want one delivery from the server's rule", sent)
	}
}
