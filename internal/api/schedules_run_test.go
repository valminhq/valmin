package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func runSchedule(t *testing.T, rt *Server, u *store.User, id string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, schedulesPath+"/"+id+"/run", http.NoBody))
}

// TestRunNowRunsTheScheduleAsItsClockWould asserts a run started by hand is the job the clock
// submits, carrying both the schedule and the person, that it is audited, and that the
// schedule's own next and last runs are left as they were.
func TestRunNowRunsTheScheduleAsItsClockWould(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	id := seededInstanceID
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	scheduleID := seedScheduleRow(t, db, "restart", &id, next)

	rec := runSchedule(t, rt, admin, scheduleID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got, want := rec.Header().Get("Location"), "/api/v1/jobs/"+stub.JobID; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if !stub.Scheduled {
		t.Error("the accepted job does not read as scheduled")
	}
	rows := jobRowsForSchedule(t, db, scheduleID)
	if len(rows) != 1 || rows[0].ID != stub.JobID || rows[0].Kind != "restart" {
		t.Fatalf("jobs of the schedule = %+v, want the one restart run now", rows)
	}
	if got := deref(rows[0].RequestedBy); got != admin.ID {
		t.Errorf("requested_by = %q, want %q", got, admin.ID)
	}
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("restart run now = %+v", final)
	}

	sc, err := db.ScheduleByID(t.Context(), scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.LastRunAt != nil || sc.NextRunAt == nil || !sc.NextRunAt.Equal(next) {
		t.Errorf("last_run_at = %v, next_run_at = %v, want nil and %v", sc.LastRunAt, sc.NextRunAt, next)
	}
	audit := auditRecordsFor(t, db, "schedules.run")
	if len(audit) != 1 || deref(audit[0].Detail) != `{"kind":"restart"}` || deref(audit[0].UserID) != admin.ID {
		t.Errorf("schedules.run entries = %+v, want one naming the restart and its runner", audit)
	}
}

// TestRunNowRunsAChain asserts a chain schedule runs as one chain job, as its clock runs it.
func TestRunNowRunsAChain(t *testing.T) {
	t.Parallel()
	rt, db, _, admin, _ := backupsWorld(t)
	created := postSchedule(t, rt, admin, chainBody(`[{"type":"wait","seconds":1}]`))
	if created.Code != http.StatusCreated {
		t.Fatalf("POST chain = %d (%s)", created.Code, created.Body)
	}
	var sc scheduleView
	decodeInto(t, created, &sc)

	rec := runSchedule(t, rt, admin, sc.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if stub.Kind != "chain" {
		t.Errorf("kind = %q, want chain", stub.Kind)
	}
	if final := waitJob(t, rt, admin, stub.JobID); final.Status != "succeeded" {
		t.Fatalf("chain run now = %+v", final)
	}
	if rows := jobRowsForSchedule(t, db, sc.ID); len(rows) != 1 || deref(rows[0].RequestedBy) != admin.ID {
		t.Errorf("jobs of the chain = %+v, want one naming its runner", rows)
	}
}

// TestRunNowRefusals asserts each run the endpoint refuses answers with its status and code
// and leaves no job behind.
func TestRunNowRefusals(t *testing.T) {
	t.Parallel()
	hold := func(t *testing.T, w backupWorld, id string) {
		if err := w.db.DeferSchedule(t.Context(), id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	lock := func(t *testing.T, w backupWorld, _ string) {
		seedStaleJob(t, w.db, "world_import", "", `{"staging_dir":""}`)
	}
	modded := func(t *testing.T, w backupWorld, _ string) { makeModded(t, w.db) }

	cases := []struct {
		name, state, kind string
		prep              func(*testing.T, backupWorld, string)
		asMember          bool
		wantCode          int
		wantErr           string
	}{
		{"a panel schedule the member cannot see", "stopped", "prune", nil, true, http.StatusNotFound, "not_found"},
		{"a step the member may not run", "running", "restart", nil, true, http.StatusForbidden, "forbidden"},
		{"a panel schedule", "stopped", "prune", nil, false, http.StatusConflict, "invalid_state"},
		{"a run held for players", "running", "restart", hold, false, http.StatusConflict, "invalid_state"},
		{"a state the kind cannot run from", "stopped", "restart", nil, false, http.StatusConflict, "invalid_state"},
		{"a server busy with another job", "stopped", "backup", lock, false, http.StatusConflict, "job_in_progress"},
		{
			"a game update of a modded server",
			"stopped",
			"game_update",
			modded,
			false,
			http.StatusConflict,
			"invalid_state",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newBackupWorld(t, c.state)
			var instanceID *string
			if c.kind != "prune" {
				id := seededInstanceID
				instanceID = &id
			}
			scheduleID := seedScheduleRow(t, w.db, c.kind, instanceID, time.Now().UTC().Add(time.Hour))
			if c.prep != nil {
				c.prep(t, w, scheduleID)
			}
			u := w.admin
			if c.asMember {
				u = w.member
			}

			rec := runSchedule(t, w.rt, u, scheduleID)
			if rec.Code != c.wantCode || errCode(t, rec) != c.wantErr {
				t.Fatalf("run = %d %s, want %d %s", rec.Code, rec.Body, c.wantCode, c.wantErr)
			}
			if instanceID != nil {
				if rows := jobRowsForSchedule(t, w.db, scheduleID); len(rows) != 0 {
					t.Errorf("a refused run left jobs %+v", rows)
				}
			}
		})
	}
}
