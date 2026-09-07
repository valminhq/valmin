package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

const schedulesPath = "/api/v1/schedules"

// schedulesOf returns the Schedules handler the router built, which is also the clock's
// enqueuer — the same object both halves of this package use.
func schedulesOf(rt *Router) *Schedules {
	return &Schedules{
		DB:    rt.Supervisor().inst.DB,
		Authz: rt.Supervisor().inst.Authz,
		// The router's own is unexported behind the scheduler; this is the same wiring.
		Instances: rt.Supervisor().inst,
	}
}

// seedScheduleRow writes a schedule directly, for the cases that start from one already due.
func seedScheduleRow(t *testing.T, db *store.DB, kind string, instanceID *string, nextRunAt time.Time) string {
	t.Helper()
	s := &store.Schedule{
		ID: store.NewID(), InstanceID: instanceID, Kind: kind, Cron: "0 3 * * *",
		Payload: "{}", Enabled: true, NextRunAt: &nextRunAt,
	}
	if err := db.CreateSchedule(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// jobRowsForSchedule reads back what a tick left in the job history.
func jobRowsForSchedule(t *testing.T, db *store.DB, scheduleID string) []store.Job {
	t.Helper()
	rows, err := db.ListJobsForInstance(t.Context(), seededInstanceID, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Job
	for i := range rows {
		if rows[i].ScheduleID != nil && *rows[i].ScheduleID == scheduleID {
			out = append(out, rows[i])
		}
	}
	return out
}

func postSchedule(t *testing.T, rt *Router, u *store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, schedulesPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return as(rt, u, req)
}

// Asserts a due backup schedule enqueues a real backup job carrying its schedule_id and no
// requester: a job nobody asked for must say so (12 §11).
func TestATickEnqueuesABackupWithNoRequester(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "backup", &id, time.Now().UTC().Add(-time.Minute))

	sched := &scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}
	sched.Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 {
		t.Fatalf("the tick produced %d job rows, want 1", len(rows))
	}
	if rows[0].Kind != "backup" {
		t.Errorf("kind = %q, want backup", rows[0].Kind)
	}
	if rows[0].RequestedBy != nil {
		t.Errorf("requested_by = %v, want NULL for a scheduled run", *rows[0].RequestedBy)
	}
	if final := waitJob(t, rt, w.admin, rows[0].ID); final.Status != "succeeded" {
		t.Fatalf("scheduled backup = %+v", final)
	}
	page := listBackupsAs(t, rt, w.admin, "")
	if len(page.Items) != 1 || page.Items[0]["trigger"] != store.TriggerScheduled {
		t.Fatalf("scheduled archive catalogue = %+v, want one scheduled archive", page.Items)
	}
}

// Asserts a tick that finds the instance lock held records a skip and enqueues nothing, and
// that the skip is in the instance's job history where an operator will find it (ADR-030).
func TestATickOnAHeldLockRecordsASkip(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "backup", &id, time.Now().UTC().Add(-time.Minute))
	held := seedStaleJob(t, db, "world_import", "", `{"staging_dir":""}`)

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 {
		t.Fatalf("the tick produced %d job rows, want exactly the skip", len(rows))
	}
	skip := rows[0]
	if skip.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", skip.Status)
	}
	if skip.ErrorCode == nil || *skip.ErrorCode != "job_in_progress" {
		t.Errorf("error_code = %v, want job_in_progress", skip.ErrorCode)
	}
	// And nothing new took the lock the interrupted job still holds.
	if jobRow(t, db, held).Status != "running" {
		t.Error("the tick disturbed the job already holding the lock")
	}
}

// Asserts a tick against an instance in a state the kind cannot be claimed from records a skip
// rather than archiving whatever is on disk.
func TestATickOnAnInstanceInErrorRecordsASkip(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	seed(t, db, `UPDATE instances SET state = 'error' WHERE id = ?`, seededInstanceID)
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "backup", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("the tick produced %v, want one recorded skip", rows)
	}
	if files := archiveFiles(t, rt); len(files) != 0 {
		t.Errorf("a scheduled backup of an instance in error wrote %v", files)
	}
}

// Asserts prune applies each instance's retention to archives that already exist, and that a
// second run over a catalogue already inside its policy deletes nothing.
func TestPruneAppliesRetentionAndIsIdempotent(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	seed(t, db, `UPDATE instances SET backup_keep_cold = 2 WHERE id = ?`, seededInstanceID)
	base := time.Now().UTC().Add(-time.Hour)
	for i, id := range []string{"b-1", "b-2", "b-3", "b-4"} {
		seedArchive(t, db, root, id, store.TriggerManual, true, base.Add(time.Duration(i)*time.Minute))
	}

	runPruneJob(t, rt, admin)
	if got := len(listBackupsAs(t, rt, admin, "").Items); got != 2 {
		t.Fatalf("catalogue holds %d archives after a prune to keep_cold = 2, want 2", got)
	}
	for _, gone := range []string{"b-1", "b-2"} {
		if _, err := os.Stat(filepath.Join(root, "backups", "inst-a", gone+".tar.gz")); !os.IsNotExist(err) {
			t.Errorf("%s survived the prune on disk", gone)
		}
	}

	runPruneJob(t, rt, admin)
	if got := len(listBackupsAs(t, rt, admin, "").Items); got != 2 {
		t.Errorf("a second prune left %d archives, want the same 2", got)
	}
}

// Asserts prune over a panel with no instances and nothing to remove succeeds rather than
// failing on the empty case.
func TestPruneOverAnEmptyPanelSucceeds(t *testing.T) {
	rt, _, _, admin, _ := backupsWorld(t)
	runPruneJob(t, rt, admin)
}

// runPruneJob submits the global prune the way a tick does and waits for it.
func runPruneJob(t *testing.T, rt *Router, admin *store.User) {
	t.Helper()
	inst := rt.Supervisor().inst
	job, err := inst.Engine.Submit(t.Context(), pruneSpec(""), inst.runPrune)
	if err != nil {
		t.Fatalf("submit prune: %v", err)
	}
	if final := waitJob(t, rt, admin, job.ID); final.Status != "succeeded" {
		t.Fatalf("prune job = %+v", final)
	}
}

// Asserts an expression the panel cannot read is refused at write time, naming the field —
// never stored and discovered at three in the morning.
func TestPostScheduleRefusesAnUnreadableExpression(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"every night please"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST with a bad expression = %d, want 422 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"cron"`) {
		t.Errorf("the 422 does not name the cron field: %s", rec.Body)
	}
	rows, err := db.ListSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a schedule was stored anyway: %v", rows)
	}
}

// Asserts a kind the panel will not put on a timer is refused, rather than stored as a row
// nothing can ever execute. `restore` is a real job kind and deliberately not a schedulable
// one: replacing a world on a schedule is not an operation anybody wants.
func TestPostScheduleRefusesAKindThisBuildCannotRun(t *testing.T) {
	rt, _, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin,
		`{"kind":"restore","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST of an unrunnable kind = %d, want 422 (%s)", rec.Code, rec.Body)
	}
}

// Asserts a member without schedules.global can neither create a global schedule nor see one
// that exists (09 §3.3, ADR-038).
func TestGlobalSchedulesAreInvisibleWithoutTheAction(t *testing.T) {
	rt, db, _, admin, member := backupsWorld(t)
	seedScheduleRow(t, db, "prune", nil, time.Now().UTC().Add(time.Hour))

	if rec := postSchedule(t, rt, member, `{"kind":"prune","cron":"0 4 * * *"}`); rec.Code != http.StatusNotFound {
		t.Errorf("member creating a global schedule = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if got := len(listSchedulesAs(t, rt, member)); got != 0 {
		t.Errorf("a member enumerated %d global schedules, want 0", got)
	}
	if got := len(listSchedulesAs(t, rt, admin)); got != 1 {
		t.Errorf("an admin enumerated %d schedules, want 1", got)
	}
}

// Asserts an instance schedule is authorized by the action its tick would exercise: a member
// who cannot take a backup cannot schedule one either (ADR-131).
func TestAnInstanceScheduleNeedsTheActionItWouldRun(t *testing.T) {
	rt, _, _, _, member := backupsWorld(t)

	rec := postSchedule(t, rt, member,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("member scheduling a backup without backups.create = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// Asserts the full round trip an operator makes: create, disable, delete.
func TestScheduleRoundTrip(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)
	if created.NextRunAt == nil {
		t.Error("a new schedule has no next_run_at, so the clock would have to guess one")
	}
	if created.Timezone != "UTC" {
		t.Errorf("timezone = %q, want the daemon's own", created.Timezone)
	}

	req := httptest.NewRequest(http.MethodPatch, schedulesPath+"/"+created.ID,
		strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := as(rt, admin, req); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if reread, err := db.ScheduleByID(t.Context(), created.ID); err != nil || reread.Enabled {
		t.Errorf("schedule is still enabled after a PATCH disabling it (%v)", err)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodDelete, schedulesPath+"/"+created.ID, http.NoBody))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if got := len(listSchedulesAs(t, rt, admin)); got != 0 {
		t.Errorf("%d schedules survived the delete", got)
	}
}

func listSchedulesAs(t *testing.T, rt *Router, u *store.User) []scheduleView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, schedulesPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /schedules = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []scheduleView `json:"items"`
	}
	decodeInto(t, rec, &page)
	return page.Items
}

// Asserts a due restart schedule submits a real restart, which leaves the server running
// again — the whole point of scheduling one.
func TestATickEnqueuesARestart(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "restart", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Kind != "restart" {
		t.Fatalf("the tick produced %v, want one restart job", rows)
	}
	if rows[0].RequestedBy != nil {
		t.Errorf("requested_by = %v, want NULL for a scheduled run", *rows[0].RequestedBy)
	}
	if final := waitJob(t, rt, admin, rows[0].ID); final.Status != "succeeded" {
		t.Fatalf("scheduled restart = %+v", final)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %q, want running after a scheduled restart", got)
	}
}

// Asserts a restart schedule that comes round while the server is already stopped records a
// skip rather than starting one nobody asked to start (12 §3.1).
func TestATickDoesNotRestartAStoppedServer(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db := w.rt, w.db
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "restart", &id, time.Now().UTC().Add(-time.Minute))

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].Status != "cancelled" {
		t.Fatalf("the tick produced %v, want one recorded skip", rows)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want the server left stopped", got)
	}
}

// Asserts a restart schedule is gated on instance.restart, the action its tick exercises
// (ADR-132).
func TestARestartScheduleNeedsInstanceRestart(t *testing.T) {
	rt, _, _, _, member := backupsWorld(t)

	rec := postSchedule(t, rt, member,
		`{"kind":"restart","instance_id":"`+seededInstanceID+`","cron":"0 5 * * *"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("member scheduling a restart without instance.restart = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// Asserts a schedule records who set it up and names them back, so an operator reading the
// list can see whose arrangement it is.
func TestAScheduleRecordsItsAuthor(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)
	if created.CreatedBy == nil || *created.CreatedBy != admin.ID {
		t.Errorf("created_by = %v, want %s", created.CreatedBy, admin.ID)
	}

	listed := listSchedulesAs(t, rt, admin)
	if len(listed) != 1 {
		t.Fatalf("listed %d schedules, want 1", len(listed))
	}
	if listed[0].CreatedByUsername == nil || *listed[0].CreatedByUsername != admin.Username {
		t.Errorf("created_by_username = %v, want %q", listed[0].CreatedByUsername, admin.Username)
	}
	if stored, err := db.ScheduleByID(t.Context(), created.ID); err != nil || stored.CreatedBy == nil {
		t.Errorf("the stored row carries no author (%v)", err)
	}
}

// Asserts created_by is audit only: a schedule keeps firing after its author's grant is
// revoked. A nightly backup that stops because somebody left the group is the failure this
// milestone exists to prevent (ADR-134).
func TestAScheduleOutlivesItsAuthorsGrant(t *testing.T) {
	w := newBackupWorld(t, "stopped")
	rt, db, member := w.rt, w.db, w.member
	seed(t, db, `UPDATE instance_grants SET role = 'operator' WHERE user_id = ?`, member.ID)

	rec := postSchedule(t, rt, member,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST as an operator = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)

	// The grant goes away entirely, which is stronger than a role change: the author can no
	// longer see the instance, let alone back it up.
	seed(t, db, `DELETE FROM instance_grants WHERE user_id = ?`, member.ID)
	seed(t, db, `UPDATE scheduled_jobs SET next_run_at = ? WHERE id = ?`,
		store.FormatTime(time.Now().UTC().Add(-time.Minute)), created.ID)

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, created.ID)
	if len(rows) != 1 || rows[0].Kind != "backup" || rows[0].Status == "cancelled" {
		t.Fatalf("the tick produced %v, want the backup to have run anyway", rows)
	}
}

// Asserts deleting the author leaves the schedule in place, unnamed: schedules belong to the
// panel, and an account being gone is not a reason to stop backing a world up.
func TestDeletingAnAuthorLeavesTheScheduleRunning(t *testing.T) {
	rt, db, _, admin, member := backupsWorld(t)
	seed(t, db, `UPDATE instance_grants SET role = 'operator' WHERE user_id = ?`, member.ID)

	rec := postSchedule(t, rt, member,
		`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	if err := db.DeleteUser(t.Context(), member.ID); err != nil {
		t.Fatal(err)
	}

	listed := listSchedulesAs(t, rt, admin)
	if len(listed) != 1 {
		t.Fatalf("deleting the author removed the schedule: %v", listed)
	}
	if listed[0].CreatedBy != nil || listed[0].CreatedByUsername != nil {
		t.Errorf("author = %v/%v, want both null once the account is gone",
			listed[0].CreatedBy, listed[0].CreatedByUsername)
	}
}
