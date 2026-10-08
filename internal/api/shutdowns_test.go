package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

const shutdownsPath = "/api/v1/admin/shutdowns"

func shutdownRequestAs(t *testing.T, rt *Server, u *store.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return as(rt, u, req)
}

// TestPlannedShutdownRoundTrip asserts an admin can plan a power cut in a time zone, see it with
// its author, and cancel it once.
func TestPlannedShutdownRoundTrip(t *testing.T) {
	rt, _, _, admin, _ := backupsWorld(t)

	rec := shutdownRequestAs(t, rt, admin, http.MethodPost, shutdownsPath,
		`{"local_time":"2099-01-01T14:00","timezone":"Europe/Kyiv"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var created shutdownView
	decodeInto(t, rec, &created)
	if want := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC); !created.PowerOffAt.Equal(want) {
		t.Errorf("power_off_at = %v, want %v", created.PowerOffAt, want)
	}

	rec = shutdownRequestAs(t, rt, admin, http.MethodGet, shutdownsPath, "")
	var page Page[shutdownView]
	decodeInto(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != created.ID ||
		page.Items[0].CreatedByUsername == nil || *page.Items[0].CreatedByUsername != admin.Username {
		t.Fatalf("list = %+v", page.Items)
	}

	for _, want := range []int{http.StatusNoContent, http.StatusNotFound} {
		rec = shutdownRequestAs(t, rt, admin, http.MethodDelete, shutdownsPath+"/"+created.ID, "")
		if rec.Code != want {
			t.Errorf("DELETE = %d, want %d (%s)", rec.Code, want, rec.Body)
		}
	}
}

// TestPlannedShutdownRefusals asserts a member sees no power cuts and cannot plan one, and that
// a past time or an unknown zone is a field error.
func TestPlannedShutdownRefusals(t *testing.T) {
	rt, _, _, admin, member := backupsWorld(t)
	tests := []struct {
		name   string
		u      *store.User
		method string
		body   string
		want   int
	}{
		{"member lists", member, http.MethodGet, "", http.StatusNotFound},
		{
			"member plans",
			member,
			http.MethodPost,
			`{"local_time":"2099-01-01T14:00","timezone":"UTC"}`,
			http.StatusNotFound,
		},
		{
			"past time",
			admin,
			http.MethodPost,
			`{"local_time":"2001-01-01T14:00","timezone":"UTC"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"unreadable time",
			admin,
			http.MethodPost,
			`{"local_time":"soon","timezone":"UTC"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"unknown zone",
			admin,
			http.MethodPost,
			`{"local_time":"2099-01-01T14:00","timezone":"Mars/Base"}`,
			http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := shutdownRequestAs(t, rt, tt.u, tt.method, shutdownsPath, tt.body); rec.Code != tt.want {
				t.Errorf("%s = %d, want %d (%s)", tt.method, rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestAPlannedShutdownStopsARunningServer asserts the clock's stop hook stops a running server
// cleanly and records the stop as the planned shutdown's.
func TestAPlannedShutdownStopsARunningServer(t *testing.T) {
	w := newBackupWorld(t, "running")
	rt, db, admin := w.rt, w.db, w.admin
	now := time.Now().UTC()
	if err := db.CreatePlannedShutdown(t.Context(),
		&store.PlannedShutdown{ID: "cut", PowerOffAt: now.Add(time.Minute)}, nil); err != nil {
		t.Fatal(err)
	}
	h := &Shutdowns{DB: db, Authz: rt.instances.Authz, Instances: rt.instances}

	(&scheduler.Shutdowns{DB: db, Stop: h.Stop, Warn: h.Warn}).Tick(t.Context(), now)

	jobs, err := db.ListJobsForInstance(t.Context(), seededInstanceID, "", "", 10, false)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "stop" {
		t.Fatalf("jobs = %+v, %v; want one stop", jobs, err)
	}
	if final := waitJob(t, rt, admin, jobs[0].ID); final.Status != "succeeded" {
		t.Fatalf("stop = %+v", final)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %q, want stopped", got)
	}
	var actor string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT actor_name FROM audit_log WHERE action = 'instances.stop'`).Scan(&actor); err != nil ||
		actor != "Planned shutdown" {
		t.Errorf("audit actor = %q, %v; want Planned shutdown", actor, err)
	}
}
