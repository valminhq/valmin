package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// The three v1 events (instance_down, update_available, backup_failed) predate alert rules and
// used to go to every destination, while a rule covering the same incident sent its own
// alert_opened as well: one incident, two alerts at one destination (05 "Post-v1 additions").
// These tests pin the fix: a destination hears about one incident once, from the rule when a
// rule routes it there and from the v1 event otherwise.

// seedRule routes one condition kind to the given destinations.
func seedRule(t *testing.T, db *store.DB, kind alerts.Kind, params string, webhookIDs ...string) {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	if err := db.SaveAlertRule(t.Context(), &store.AlertRule{
		ID: store.NewID(), ConditionKind: kind.String(), Params: params, Enabled: true,
		WebhookIDs: webhookIDs,
	}); err != nil {
		t.Fatal(err)
	}
}

// deliveriesTo reads every delivery row one destination was owed, by event kind.
func deliveriesTo(t *testing.T, db *store.DB, webhookID string) map[string]int {
	t.Helper()
	all, err := db.ListDeliveries(t.Context(), "", "", 100)
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
func scanNow(t *testing.T, rt *Router) {
	t.Helper()
	if _, err := rt.Supervisor().inst.scanAlerts(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// failJob records a terminal failed job and runs the finish hook the engine runs with it.
func failJob(t *testing.T, rt *Router, db *store.DB, kind jobs.Kind, instanceID string) {
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

// TestAFailedBackupReachesARuleDestinationOnce is the regression: a job_failed rule and the v1
// backup_failed event describe the same failure, so the rule's destination hears it once.
func TestAFailedBackupReachesARuleDestinationOnce(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindJobFailed, "", ruled)
	instanceID := seedStoppedInstance(t, db, "backup-fail").ID

	failJob(t, rt, db, jobs.KindBackup, instanceID)
	scanNow(t, rt)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindAlertOpened.String(): 1})
	assertDeliveries(t, db, "a destination no rule names", everyone,
		map[string]int{notify.KindBackupFailed.String(): 1})
}

// TestABackupFailureTheRuleCannotAnnounceStillArrives asserts the other half: when the rule's
// condition is already open, the scan opens no new edge, so the v1 event is the only word the
// destination would get and it is not withheld.
func TestABackupFailureTheRuleCannotAnnounceStillArrives(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	seedRule(t, db, alerts.KindJobFailed, "", ruled)
	instanceID := seedStoppedInstance(t, db, "restore-then-backup").ID

	failJob(t, rt, db, jobs.KindRestore, instanceID)
	scanNow(t, rt)
	failJob(t, rt, db, jobs.KindBackup, instanceID)
	scanNow(t, rt)

	assertDeliveries(t, db, "the rule's destination", ruled, map[string]int{
		notify.KindAlertOpened.String():  1,
		notify.KindBackupFailed.String(): 1,
	})
}

// TestANewBuildReachesARuleDestinationOnce is the same regression for update_available.
func TestANewBuildReachesARuleDestinationOnce(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindUpdateAvailable, "", ruled)
	inst := seedStoppedInstance(t, db, "behind")
	seed(t, db, `UPDATE instances SET game_build_id = '21981590' WHERE id = ?`, inst.ID)

	owed := rt.webhooks.NotifyPublicBuild(t.Context(), "21981590", "22000000")
	if owed == nil {
		t.Fatal("a new public build owes no notification")
	}
	runInTx(t, db, owed)
	if err := db.KVSet(t.Context(), publicBuildKey,
		publicBuild{BuildID: "22000000", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	scanNow(t, rt)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindAlertOpened.String(): 1})
	assertDeliveries(t, db, "a destination no rule names", everyone,
		map[string]int{notify.KindUpdateAvailable.String(): 1})
}

// TestAnUnexpectedStopReachesARuleDestinationOnce is the regression for instance_down, against
// a crash-loop rule tuned so this one stop is the loop.
func TestAnUnexpectedStopReachesARuleDestinationOnce(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	ruled := seedWebhook(t, db, "ruled")
	everyone := seedWebhook(t, db, "everyone")
	seedRule(t, db, alerts.KindCrashLoop, `{"crash_count":1,"crash_window_seconds":600}`, ruled)
	containerID := seedInstance(t, rt, db, fake, "running")

	fake.Get(containerID).Exit(0)
	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	scanNow(t, rt)

	assertDeliveries(t, db, "the rule's destination", ruled,
		map[string]int{notify.KindAlertOpened.String(): 1})
	assertDeliveries(t, db, "a destination no rule names", everyone,
		map[string]int{notify.KindInstanceDown.String(): 1})
}
