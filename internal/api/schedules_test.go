package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
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
		MaxDeferral: store.DefaultMaxDeferral, UnknownPlayers: store.UnknownPlayersWait,
	}
	if err := db.CreateSchedule(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// jobRowsForSchedule reads back what a tick left in the job history.
func jobRowsForSchedule(t *testing.T, db *store.DB, scheduleID string) []store.Job {
	t.Helper()
	return jobRowsForScheduleOn(t, db, seededInstanceID, scheduleID)
}

func jobRowsForScheduleOn(t *testing.T, db *store.DB, instanceID, scheduleID string) []store.Job {
	t.Helper()
	rows, err := db.ListJobsForInstance(t.Context(), instanceID, "", "", 50, true)
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

// Asserts an expression the panel cannot read, or one naming its own timezone instead of the
// UTC every row is labelled with, is refused at write time naming the field and never stored.
func TestPostScheduleRefusesAnUnreadableExpression(t *testing.T) {
	for _, expr := range []string{
		"every night please",
		"CRON_TZ=Europe/Oslo 0 4 * * *",
		"TZ=Asia/Tokyo @daily",
	} {
		t.Run(expr, func(t *testing.T) {
			rt, db, _, admin, _ := backupsWorld(t)

			rec := postSchedule(t, rt, admin,
				`{"kind":"backup","instance_id":"`+seededInstanceID+`","cron":"`+expr+`"}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("POST with %q = %d, want 422 (%s)", expr, rec.Code, rec.Body)
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
		})
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
	if len(created.UpcomingRuns) != upcomingRunCount ||
		(created.NextRunAt != nil && !created.UpcomingRuns[0].Equal(*created.NextRunAt)) {
		t.Errorf("upcoming_runs = %v, want %d starting at next_run_at", created.UpcomingRuns, upcomingRunCount)
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
	t.Parallel()
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

func TestEmptyScheduleListIncludesTimezone(t *testing.T) {
	rt, _, _, admin, _ := backupsWorld(t)
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, schedulesPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET schedules = %d, want 200", rec.Code)
	}
	var result struct {
		Page[scheduleView]
		Timezone string `json:"timezone"`
	}
	decodeInto(t, rec, &result)
	if result.Timezone != "UTC" || result.Items == nil || len(result.Items) != 0 {
		t.Fatalf("empty schedules = %+v, want an empty list with UTC timezone", result)
	}
}

// Asserts upcoming runs keep a future next_run_at first, skip a held run's past one, and are
// empty for a disabled schedule or an unreadable expression.
func TestUpcomingRuns(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	future := time.Date(2026, 9, 28, 10, 45, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	hour := func(h int) time.Time { return time.Date(2026, 9, 28, h, 0, 0, 0, time.UTC) }
	hourly := []time.Time{hour(11), hour(12), hour(13), hour(14), hour(15)}
	tests := []struct {
		name    string
		cron    string
		enabled bool
		next    *time.Time
		want    []time.Time
	}{
		{"hourly, no next_run_at", "@hourly", true, nil, hourly},
		{"future next_run_at first", "@hourly", true, &future, append([]time.Time{future}, hourly[:4]...)},
		{"held run's past next_run_at skipped", "@hourly", true, &past, hourly},
		{"disabled", "@hourly", false, &future, []time.Time{}},
		{"unreadable expression", "every night please", true, nil, []time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upcomingRuns(&store.Schedule{Cron: tt.cron, Enabled: tt.enabled, NextRunAt: tt.next}, now)
			if got == nil || !slices.EqualFunc(got, tt.want, time.Time.Equal) {
				t.Errorf("upcomingRuns = %v, want %v", got, tt.want)
			}
		})
	}
}

// Asserts only a running server can be occupied, and an unknown count follows the schedule.
func TestOccupied(t *testing.T) {
	zero, two := 0, 2
	tests := []struct {
		name    string
		state   instance.State
		players *int
		unknown string
		want    bool
	}{
		{"running, empty", instance.StateRunning, &zero, store.UnknownPlayersWait, false},
		{"running, two players", instance.StateRunning, &two, store.UnknownPlayersRun, true},
		{"running, unknown, wait", instance.StateRunning, nil, store.UnknownPlayersWait, true},
		{"running, unknown, run", instance.StateRunning, nil, store.UnknownPlayersRun, false},
		{"stopped, two players", instance.StateStopped, &two, store.UnknownPlayersWait, false},
		{"stopped, unknown, wait", instance.StateStopped, nil, store.UnknownPlayersWait, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := occupied(tt.state, tt.players, tt.unknown); got != tt.want {
				t.Errorf("occupied = %v, want %v", got, tt.want)
			}
		})
	}
}

// Asserts the chat warning names what the run does and how long players have, rounded up to
// whole minutes.
func TestPlayerWarning(t *testing.T) {
	tests := []struct {
		kind string
		left time.Duration
		want string
	}{
		{
			"restart", 2 * time.Hour,
			"The server restarts when all players have left, or in 2 hours at the latest.",
		},
		{
			"backup", 4*time.Minute + 30*time.Second,
			"The server restarts for a backup when all players have left, or in 5 minutes at the latest.",
		},
		{
			"restart", time.Minute,
			"The server restarts when all players have left, or in 1 minute at the latest.",
		},
		{
			"restart", 90 * time.Minute,
			"The server restarts when all players have left, or in 90 minutes at the latest.",
		},
		{
			"restart", time.Hour,
			"The server restarts when all players have left, or in 1 hour at the latest.",
		},
	}
	for _, tt := range tests {
		if got := playerWarning(tt.kind, tt.left); got != tt.want {
			t.Errorf("playerWarning(%s, %s) = %q, want %q", tt.kind, tt.left, got, tt.want)
		}
	}
}

// Asserts a schedule's player policy round-trips, and defaults when the body leaves it out.
func TestSchedulePlayerPolicyRoundTrip(t *testing.T) {
	tests := []struct {
		name, policy string
		wait         bool
		maxSecs      int64
		unknown      string
	}{
		{"defaults", "", false, 7200, "wait"},
		{"set", `,"wait_for_empty":true,"max_deferral_seconds":900,"unknown_players":"run"`, true, 900, "run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, _, admin, _ := backupsWorld(t)
			rec := postSchedule(t, rt, admin,
				`{"kind":"restart","instance_id":"`+seededInstanceID+`","cron":"0 3 * * *"`+tt.policy+`}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
			}
			listed := listSchedulesAs(t, rt, admin)
			if len(listed) != 1 {
				t.Fatalf("listed %d schedules, want 1", len(listed))
			}
			got := listed[0]
			if got.WaitForEmpty != tt.wait || got.MaxDeferralSeconds != tt.maxSecs || got.UnknownPlayers != tt.unknown {
				t.Errorf("policy = %v/%d/%q, want %v/%d/%q", got.WaitForEmpty, got.MaxDeferralSeconds,
					got.UnknownPlayers, tt.wait, tt.maxSecs, tt.unknown)
			}
			if got.DeferredSince != nil || got.DeferredUntil != nil {
				t.Errorf("deferred = %v/%v, want null for a schedule holding nothing",
					got.DeferredSince, got.DeferredUntil)
			}
		})
	}
}

// Asserts an invalid player policy is refused naming the field, and never stored.
func TestPostScheduleRefusesAnInvalidPlayerPolicy(t *testing.T) {
	tests := []struct {
		name, kind, policy, field string
	}{
		{"wait on game_update", "game_update", `"wait_for_empty":true`, "wait_for_empty"},
		{"deferral too short", "restart", `"max_deferral_seconds":59`, "max_deferral_seconds"},
		{"deferral too long", "backup", `"max_deferral_seconds":86401`, "max_deferral_seconds"},
		{"unknown players", "restart", `"unknown_players":"maybe"`, "unknown_players"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, _, admin, _ := backupsWorld(t)
			rec := postSchedule(t, rt, admin, `{"kind":"`+tt.kind+`","instance_id":"`+seededInstanceID+
				`","cron":"0 3 * * *",`+tt.policy+`}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("POST = %d, want 422 (%s)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"`+tt.field+`"`) {
				t.Errorf("the 422 does not name %s: %s", tt.field, rec.Body)
			}
			if rows, err := db.ListSchedules(t.Context()); err != nil || len(rows) != 0 {
				t.Errorf("stored %v (%v), want nothing", rows, err)
			}
		})
	}
}

// Asserts disabling a schedule that is holding a run releases the hold, and re-enabling it
// skips the missed run.
func TestDisablingAHeldScheduleClearsTheHold(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)
	id := seededInstanceID
	scheduleID := seedScheduleRow(t, db, "restart", &id, time.Now().UTC().Add(-time.Minute))
	seed(t, db, `UPDATE scheduled_jobs SET wait_for_empty = TRUE WHERE id = ?`, scheduleID)
	if err := db.DeferSchedule(t.Context(), scheduleID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if listed := listSchedulesAs(t, rt, admin); len(listed) != 1 || listed[0].DeferredUntil == nil {
		t.Fatalf("listed %+v, want one held schedule with a deferred_until", listed)
	}

	req := httptest.NewRequest(http.MethodPatch, schedulesPath+"/"+scheduleID,
		strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := as(rt, admin, req); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if reread, err := db.ScheduleByID(t.Context(), scheduleID); err != nil || reread.DeferredSince != nil {
		t.Errorf("deferred_since = %v (%v), want it cleared", reread.DeferredSince, err)
	}

	req = httptest.NewRequest(http.MethodPatch, schedulesPath+"/"+scheduleID,
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := as(rt, admin, req); rec.Code != http.StatusOK {
		t.Fatalf("re-enabling PATCH = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if reread, err := db.ScheduleByID(t.Context(), scheduleID); err != nil || !reread.NextRunAt.After(time.Now()) {
		t.Errorf("next_run_at = %v (%v) after re-enabling, want the next occurrence", reread.NextRunAt, err)
	}
}
