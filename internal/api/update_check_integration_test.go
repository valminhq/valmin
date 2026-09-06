//go:build integration

package api

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/scheduler"
)

func TestScheduledUpdateCheckWithDocker(t *testing.T) {
	rt, db, _, admin := lifecycleRouter(t)
	scheduleID := seedScheduleRow(t, db, "update_check", nil, time.Now().Add(-time.Minute))
	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now())
	var id string
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT id FROM job_runs WHERE schedule_id=?`, scheduleID).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := waitForJobTerminal(t, rt, admin, id); got.Status != "succeeded" {
		t.Fatalf("SteamCMD job = %+v", got)
	}
	var observed publicBuild
	if found, err := db.KVGet(t.Context(), publicBuildKey, &observed); err != nil || !found {
		t.Fatalf("observation found=%v: %v", found, err)
	}
	if observed.BuildID != "21981590" || observed.ObservedAt.IsZero() {
		t.Fatalf("observation = %+v", observed)
	}
}
