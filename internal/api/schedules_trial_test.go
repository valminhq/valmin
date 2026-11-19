package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// updateTestScheduleBody is a create body for an update_test schedule on the seeded server.
func updateTestScheduleBody(extra string) string {
	return `{"kind":"update_test","instance_id":"` + seededInstanceID + `","cron":"0 4 * * *"` + extra + `}`
}

// TestAnUpdateTestScheduleCarriesItsApplyOption asserts apply is stored, shown, edited and
// audited for an update_test schedule, and that the kind needs instance.update.
func TestAnUpdateTestScheduleCarriesItsApplyOption(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	if rec := postSchedule(t, rt, member, updateTestScheduleBody("")); rec.Code != http.StatusForbidden {
		t.Errorf("member creating an update_test schedule = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec := postSchedule(t, rt, admin, updateTestScheduleBody(`,"apply":true`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)
	if created.Apply == nil || !*created.Apply {
		t.Fatalf("apply = %v, want true", created.Apply)
	}

	rec = patchSchedule(t, rt, admin, created.ID, `{"apply":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var edited scheduleView
	decodeInto(t, rec, &edited)
	if edited.Apply == nil || *edited.Apply {
		t.Errorf("apply after the edit = %v, want false", edited.Apply)
	}
	audit := auditRecordsFor(t, db, "schedules.update")
	if len(audit) != 1 || !strings.Contains(deref(audit[0].Detail), `"apply"`) {
		t.Errorf("schedules.update entries = %+v, want one naming apply", audit)
	}
}

// TestOnlyAnUpdateTestScheduleTakesApply asserts apply on any other kind is refused.
func TestOnlyAnUpdateTestScheduleTakesApply(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := postSchedule(t, rt, admin,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *","apply":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a backup schedule with apply = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if views := listSchedulesAs(t, rt, admin); len(views) != 0 || views == nil {
		t.Errorf("schedules after the refusal = %+v, want none", views)
	}
}

// TestAnUpdateTestScheduleRunsAtMostHourly asserts an update_test schedule cannot fire every
// few minutes.
func TestAnUpdateTestScheduleRunsAtMostHourly(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	rec := postSchedule(t, rt, admin,
		`{"kind":"update_test","instance_id":"`+seededInstanceID+`","cron":"*/10 * * * *"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an update_test every 10 minutes = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// TestAnUpdateTestWithNothingPendingIsSkipped asserts a due update_test schedule on a server
// whose game is current and whose mods have no updates records a skip and copies nothing, and
// that running it by hand is refused.
func TestAnUpdateTestWithNothingPendingIsSkipped(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `UPDATE instances SET game_build_id = '21981590' WHERE id = ?`, seededInstanceID)
	if err := db.KVSet(t.Context(), instance.PublicBuildKey,
		instance.PublicBuild{BuildID: "21981590", ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "update_test", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("the tick produced %+v, want one recorded skip", rows)
	}
	if got := deref(rows[0].Error); !strings.Contains(got, "no game or mod updates are pending") {
		t.Errorf("skip reason = %q", got)
	}
	if testCopy, err := db.InstanceTestCopy(t.Context(), seededInstanceID); err != nil || testCopy != nil {
		t.Errorf("test copy = %v (%v), want none", testCopy, err)
	}

	if rec := runSchedule(t, rt, admin, scheduleID); rec.Code != http.StatusConflict {
		t.Errorf("run now with nothing pending = %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

// TestAScheduledUpdateTestIsRefusedWhileACopyExists asserts a due update_test schedule on a
// server that already has a copy records a skip rather than a second copy.
func TestAScheduledUpdateTestIsRefusedWhileACopyExists(t *testing.T) {
	t.Parallel()
	rt, db, fake, _, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, test_of, created_at, updated_at
	) VALUES ('copy-a', 'inst-a update test', 'stopped', '/srv/copy-a', 2556, 'Server', 'World', 'x',
		'cp-copy-a', ?, ?, ?)`, seededInstanceID, store.Now(), store.Now())
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "update_test", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("the tick produced %+v, want one recorded skip", rows)
	}
	if got := deref(rows[0].Error); !strings.Contains(got, "already exists") {
		t.Errorf("skip reason = %q", got)
	}
}
