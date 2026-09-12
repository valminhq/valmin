package api

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// seedWebhook inserts an enabled destination whose URL is a real envelope, so the delivery
// path a test drives is the one the daemon runs.
func seedWebhook(t *testing.T, db *store.DB, name string) string {
	t.Helper()
	id := store.NewID()
	envelope, err := rotateKeeper(t).Encrypt(
		crypto.PurposeWebhookURL, crypto.WebhookURLLocation(id),
		[]byte("https://example.com/api/webhooks/1/t"))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `INSERT INTO webhooks (id, name, kind, url, enabled, created_at, updated_at)
		VALUES (?, ?, 'generic', ?, TRUE, ?, ?)`, id, name, envelope, store.Now(), store.Now())
	return id
}

// deliveriesOfKind reads the delivery rows an event kind produced, whatever became of them:
// the row is written with the change that owes it, and the send is a later job.
func deliveriesOfKind(t *testing.T, db *store.DB, kind notify.Kind) []store.Delivery {
	t.Helper()
	all, err := db.ListDeliveries(t.Context(), "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Delivery
	for i := range all {
		if all[i].EventKind == kind.String() {
			out = append(out, all[i])
		}
	}
	return out
}

// TestABackupFailureOwesEveryDestinationANotification asserts the event that matters most: a
// backup that failed is the one nobody finds out about on their own. The intent is written in
// the job's own finish transaction, and a backup that succeeded says nothing.
func TestABackupFailureOwesEveryDestinationANotification(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	destination := seedWebhook(t, db, "ops")
	instanceID := seedStoppedInstance(t, db, "backup-fail").ID

	finishJob(t, rt, db, &jobs.FinishedJob{
		ID: store.NewID(), Kind: jobs.KindBackup, InstanceID: &instanceID,
		InstanceName: "Midgard", Status: jobs.StatusSucceeded,
	})
	if got := deliveriesOfKind(t, db, notify.KindBackupFailed); len(got) != 0 {
		t.Fatalf("a succeeded backup owed %d notifications, want none", len(got))
	}

	finishJob(t, rt, db, &jobs.FinishedJob{
		ID: store.NewID(), Kind: jobs.KindBackup, InstanceID: &instanceID,
		InstanceName: "Midgard", Status: jobs.StatusFailed,
	})
	got := deliveriesOfKind(t, db, notify.KindBackupFailed)
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want one per enabled destination", len(got))
	}
	if got[0].WebhookID != destination {
		t.Errorf("delivery is against %s, want the enabled destination", got[0].WebhookID)
	}
	if got[0].InstanceID == nil || *got[0].InstanceID != instanceID {
		t.Errorf("delivery names instance %v, want %s", got[0].InstanceID, instanceID)
	}
}

// finishJob runs the engine's finish hook the way a terminal job does, in its own transaction.
func finishJob(t *testing.T, rt *Router, db *store.DB, fin *jobs.FinishedJob) {
	t.Helper()
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := rt.webhooks.OnJobFinished(t.Context(), tx, fin); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAnUnchangedBuildObservationSaysNothing asserts that the hourly check notifies once about
// a new build rather than every hour until someone updates.
func TestAnUnchangedBuildObservationSaysNothing(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	seedWebhook(t, db, "ops")

	if owed := rt.webhooks.NotifyPublicBuild(t.Context(), "21981590", "21981590"); owed != nil {
		t.Error("an unchanged observation owes a notification")
	}
	owed := rt.webhooks.NotifyPublicBuild(t.Context(), "21981590", "22000000")
	if owed == nil {
		t.Fatal("a new public build owes no notification")
	}
	runInTx(t, db, owed)

	got := deliveriesOfKind(t, db, notify.KindUpdateAvailable)
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want one", len(got))
	}
}

// runInTx runs an owed notification's write the way its job's finish transaction does.
func runInTx(t *testing.T, db *store.DB, fn func(context.Context, *sql.Tx) error) {
	t.Helper()
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAnExpectedStopIsQuietAndAnUnexpectedOneIsNot asserts 05 M6's rule that an operator's own
// stop wakes nobody. The quiet is structural: a stop job holds the instance lock, and the
// observer never writes while one is held (C14).
func TestAnExpectedStopIsQuietAndAnUnexpectedOneIsNot(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	seedWebhook(t, db, "ops")
	containerID := seedInstance(t, rt, db, fake, "running")

	seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		jobs.InstanceLockKey(staleJobInstance), store.NewID(), store.Now())
	fake.Get(containerID).Exit(0)
	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := deliveriesOfKind(t, db, notify.KindInstanceDown); len(got) != 0 {
		t.Fatalf("an expected stop owed %d notifications, want none", len(got))
	}

	seed(t, db, `DELETE FROM job_locks WHERE lock_key = ?`, jobs.InstanceLockKey(staleJobInstance))
	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := deliveriesOfKind(t, db, notify.KindInstanceDown)
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want one for the server that went down on its own", len(got))
	}
}
