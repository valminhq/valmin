package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

func chainBody(steps string) string {
	return `{"kind":"chain","instance_id":"` + seededInstanceID + `","cron":"0 4 * * *","steps":` + steps + `}`
}

// TestChainScheduleRoundTrip asserts a chain is stored, listed and edited with its steps, and
// that an edit replaces the whole list.
func TestChainScheduleRoundTrip(t *testing.T) {
	t.Parallel()
	rt, _, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin, chainBody(
		`[{"type":"command","command":"  say Maintenance"},{"type":"wait","seconds":300},`+
			`{"type":"stop"},{"type":"game_update"},{"type":"start"}]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST chain = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)
	if created.Kind != "chain" || len(created.Steps) != 5 || created.Steps[0].Command != "say Maintenance" {
		t.Fatalf("created chain = %+v, want five steps with the command trimmed", created)
	}

	var listed *scheduleView
	for _, v := range listSchedulesAs(t, rt, admin) {
		if v.ID == created.ID {
			listed = &v
		}
	}
	if listed == nil || len(listed.Steps) != 5 {
		t.Fatalf("listed chain = %+v, want its five steps", listed)
	}

	rec = patchSchedule(t, rt, admin, created.ID, `{"steps":[{"type":"restart"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH steps = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var edited scheduleView
	decodeInto(t, rec, &edited)
	if len(edited.Steps) != 1 || edited.Steps[0].Type != "restart" || edited.Cron != "0 4 * * *" {
		t.Errorf("edited chain = %+v, want the one restart step on the same cron", edited)
	}
}

// TestChainScheduleRefusesBadSteps asserts the field each refused body is reported against.
func TestChainScheduleRefusesBadSteps(t *testing.T) {
	t.Parallel()
	rt, _, _, admin, _ := backupsWorld(t)
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"no steps", `{"kind":"chain","instance_id":"` + seededInstanceID + `","cron":"0 4 * * *"}`, "steps"},
		{"empty list", chainBody(`[]`), "steps"},
		{"unknown type", chainBody(`[{"type":"reboot"}]`), "steps[0].type"},
		{"wait too long", chainBody(`[{"type":"wait","seconds":99999}]`), "steps[0].seconds"},
		{"multi-line command", chainBody(`[{"type":"command","command":"save\nban x"}]`), "steps[0].command"},
		{
			"steps on another kind",
			`{"kind":"backup","instance_id":"` + seededInstanceID + `","cron":"0 4 * * *","steps":[{"type":"stop"}]}`,
			"steps",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postSchedule(t, rt, admin, tt.body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tt.field) {
				t.Errorf("POST = %d %s, want 422 naming %s", rec.Code, rec.Body, tt.field)
			}
		})
	}
}

// TestEditingAChainsStepsKeepsTheOnceAnHourLimit asserts a chain that only sends commands may
// run often, and that an edit adding a step that stops the server is refused on such a cron.
func TestEditingAChainsStepsKeepsTheOnceAnHourLimit(t *testing.T) {
	t.Parallel()
	rt, _, _, admin, _ := backupsWorld(t)

	rec := postSchedule(t, rt, admin,
		`{"kind":"chain","instance_id":"`+seededInstanceID+`","cron":"*/5 * * * *",`+
			`"steps":[{"type":"command","command":"save"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST command-only chain every 5 minutes = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)

	rec = patchSchedule(t, rt, admin, created.ID, `{"steps":[{"type":"restart"}]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cron") {
		t.Errorf("PATCH adding a restart = %d %s, want 422 on cron", rec.Code, rec.Body)
	}
}

// TestChainScheduleNeedsTheActionOfEveryStep asserts a chain can be written only by someone who
// could take each of its steps by hand, and that an operator cannot reach into one an
// administrator wrote.
func TestChainScheduleNeedsTheActionOfEveryStep(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "stopped")
	rt, db, admin, member := w.rt, w.db, w.admin, w.member

	for _, steps := range []string{`[{"type":"stop"}]`, `[{"type":"wait","seconds":5}]`} {
		if rec := postSchedule(t, rt, member, chainBody(steps)); rec.Code != http.StatusForbidden {
			t.Errorf("viewer scheduling %s = %d, want 403 (%s)", steps, rec.Code, rec.Body)
		}
	}
	seed(t, db, `UPDATE instance_grants SET role = 'operator' WHERE user_id = ?`, member.ID)
	tests := []struct {
		name  string
		steps string
		want  int
	}{
		{
			"operator steps",
			`[{"type":"command","command":"save"},{"type":"stop"},{"type":"backup"},{"type":"start"}]`,
			http.StatusCreated,
		},
		{"a command outside the allowlist", `[{"type":"command","command":"say hello"}]`, http.StatusForbidden},
		{"a game update", `[{"type":"stop"},{"type":"game_update"}]`, http.StatusForbidden},
	}
	for _, tt := range tests {
		if rec := postSchedule(t, rt, member, chainBody(tt.steps)); rec.Code != tt.want {
			t.Errorf("operator posting %s = %d, want %d (%s)", tt.name, rec.Code, tt.want, rec.Body)
		}
	}

	rec := postSchedule(t, rt, admin, chainBody(`[{"type":"game_update"}]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin POST = %d (%s)", rec.Code, rec.Body)
	}
	var adminChain scheduleView
	decodeInto(t, rec, &adminChain)
	if rec := patchSchedule(t, rt, member, adminChain.ID, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Errorf("operator disabling a chain with a game update = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	del := httptest.NewRequest(http.MethodDelete, schedulesPath+"/"+adminChain.ID, http.NoBody)
	if rec := as(rt, member, del); rec.Code != http.StatusForbidden {
		t.Errorf("operator deleting a chain with a game update = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestATickRunsAChain asserts a due chain schedule runs its steps as the jobs they normally are,
// each carrying the schedule's id.
func TestATickRunsAChain(t *testing.T) {
	t.Parallel()
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	rt.instances.ctl.Chainer.Poll = 20 * time.Millisecond

	rec := postSchedule(t, rt, admin, chainBody(`[{"type":"stop"},{"type":"start"}]`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST chain = %d (%s)", rec.Code, rec.Body)
	}
	var created scheduleView
	decodeInto(t, rec, &created)
	seed(t, db, `UPDATE scheduled_jobs SET next_run_at = ? WHERE id = ?`,
		store.FormatTime(time.Now().UTC().Add(-time.Minute)), created.ID)

	(&scheduler.Scheduler{DB: db, Enqueue: schedulesOf(rt).Enqueue}).Tick(t.Context(), time.Now().UTC())

	rows := jobRowsForSchedule(t, db, created.ID)
	var chainJob *store.Job
	for i := range rows {
		if rows[i].Kind == "chain" {
			chainJob = &rows[i]
		}
	}
	if chainJob == nil {
		t.Fatalf("the tick produced %v, want a chain job", rows)
	}
	if final := waitJob(t, rt, admin, chainJob.ID); final.Status != "succeeded" {
		t.Fatalf("scheduled chain = %+v", final)
	}
	carried := jobRowsForSchedule(t, db, created.ID)
	kinds := make([]string, 0, len(carried))
	for i := range carried {
		kinds = append(kinds, carried[i].Kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"chain", "start", "stop"}) {
		t.Errorf("jobs carrying the schedule = %v, want chain, start and stop", kinds)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %q, want running after the chain", got)
	}
}
