package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func postQueue(t *testing.T, rt *Server, u *store.User, fullName, version string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/queue",
		jsonBody(t, installBody(fullName, version))))
}

func listQueue(t *testing.T, rt *Server, u *store.User) []queuedModView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/instances/inst-a/mods/queue", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list queue = %d (%s)", rec.Code, rec.Body)
	}
	var view modQueueView
	decodeInto(t, rec, &view)
	return view.Queued
}

func storedState(t *testing.T, db *store.DB) string {
	t.Helper()
	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func countRows(t *testing.T, db *store.DB, query string) int {
	t.Helper()
	var n int
	if err := db.Reader.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lastJobOfKind is the id of the newest job of kind on inst-a.
func lastJobOfKind(t *testing.T, db *store.DB, kind string) string {
	t.Helper()
	var id string
	if err := db.Reader.QueryRowContext(t.Context(), `
		SELECT id FROM job_runs WHERE instance_id = 'inst-a' AND kind = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, kind).Scan(&id); err != nil {
		t.Fatalf("no %s job: %v", kind, err)
	}
	return id
}

// TestModQueueEndpoints asserts a queued install is listed, that queueing a package again
// replaces its version rather than adding a row, that it can be removed once, and that
// queueing needs mods.manage.
func TestModQueueEndpoints(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)

	if rec := postQueue(t, rt, member, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusForbidden {
		t.Errorf("member without mods.manage: status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	for _, version := range []string{"1.6.0", "1.7.0"} {
		if rec := postQueue(t, rt, admin, "OdinPlus-OdinArchitect", version); rec.Code != http.StatusCreated {
			t.Fatalf("queue %s: status = %d, want 201 (%s)", version, rec.Code, rec.Body)
		}
	}
	queued := listQueue(t, rt, admin)
	if len(queued) != 1 || queued[0].Version != "1.7.0" {
		t.Fatalf("queue = %+v, want OdinArchitect 1.7.0 alone", queued)
	}

	del := func() int {
		return as(rt, admin, httptest.NewRequest(http.MethodDelete,
			"/api/v1/instances/inst-a/mods/queue/OdinPlus-OdinArchitect", http.NoBody)).Code
	}
	if code := del(); code != http.StatusNoContent {
		t.Errorf("first delete = %d, want 204", code)
	}
	if code := del(); code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", code)
	}
}

// TestQueuedInstallRunsOnceTheServerIsStopped asserts the queue leaves a running server alone
// and installs, credited to whoever queued it, once the server is stopped.
func TestQueuedInstallRunsOnceTheServerIsStopped(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	queue := rt.instances.ctl.Supervisor.ModQueue
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	if rec := postQueue(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}

	queue.Drain(t.Context())
	if n := countRows(t, db, `SELECT COUNT(*) FROM job_runs WHERE kind = 'mod_install'`); n != 0 {
		t.Fatalf("%d install jobs ran against a running server", n)
	}

	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	queue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "succeeded" {
		t.Fatalf("queued install = %+v, want succeeded", got)
	}
	if rows := installedRows(t, db); len(rows) != 3 {
		t.Errorf("instance_mods = %+v, want the 3-package closure", rows)
	}
	if queued := listQueue(t, rt, admin); len(queued) != 0 {
		t.Errorf("queue = %+v, want empty", queued)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log
		WHERE action = 'instances.mods.install' AND user_id = '`+admin.ID+`'`); n != 1 {
		t.Errorf("%d install audit entries for the queuing user, want 1", n)
	}
}

// TestRestartWithQueuedInstallsStopsInstallsThenStarts asserts a restart hands a server with
// queued installs to the queue, which starts it again once they ran, even when one failed.
func TestRestartWithQueuedInstallsStopsInstallsThenStarts(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")
	savesOnStop(fake)
	queue := rt.instances.ctl.Supervisor.ModQueue
	if rec := postQueue(t, rt, admin, "Ns-NotIndexed", "1.0.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/restart", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "succeeded" {
		t.Fatalf("restart = %+v, want succeeded", got)
	}
	if got := storedState(t, db); got != "stopped" {
		t.Fatalf("state after restart = %q, want stopped while installs are queued", got)
	}

	queue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "failed" {
		t.Fatalf("install of a package not in the index = %+v, want failed", got)
	}
	queue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "start")); got.Status != "succeeded" {
		t.Fatalf("owed start = %+v, want succeeded", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for storedState(t, db) != "running" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := storedState(t, db); got != "running" || !fake.Get(containerID).Running {
		t.Errorf("state = %q, container running = %v, want running", got, fake.Get(containerID).Running)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM queued_mod_starts`); n != 0 {
		t.Errorf("%d owed starts left, want none", n)
	}
}
