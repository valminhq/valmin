package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/manager"
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

// loadMember reads the member user every installWorld seeds.
func loadMember(t *testing.T, db *store.DB) *store.User {
	t.Helper()
	u, err := db.UserByID(t.Context(), "u-member")
	if err != nil || u == nil {
		t.Fatalf("load u-member: %v", err)
	}
	return u
}

func postQueueUpdates(
	t *testing.T,
	rt *Server,
	u *store.User,
	targets []manager.UpdateTarget,
) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/mods/updates/queue",
		jsonBody(t, applyUpdatesRequest{Targets: targets})))
}

// queueAllUpdates queues every update the preview offers, as Update all does on a running server.
func queueAllUpdates(t *testing.T, rt *Server, db *store.DB, u *store.User) {
	t.Helper()
	targets := previewUpdatesOf(t, rt, u).Targets
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	rec := postQueueUpdates(t, rt, u, targets)
	if rec.Code != http.StatusCreated {
		t.Fatalf("queue updates = %d (%s)", rec.Code, rec.Body)
	}
	var view modQueueView
	decodeInto(t, rec, &view)
	queued := map[string]string{}
	for _, q := range view.Queued {
		queued[q.FullName] = q.Version
	}
	for _, target := range targets {
		if queued[target.FullName] != target.Version {
			t.Fatalf("queue = %+v, want %s %s in it", view.Queued, target.FullName, target.Version)
		}
	}
}

// TestQueueModUpdatesValidatesEveryTarget asserts the update set is refused whole when one
// target isn't a valid update, and that queueing it needs mods.manage.
func TestQueueModUpdatesValidatesEveryTarget(t *testing.T) {
	rt, db, admin, _ := updateWorld(t)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	member := loadMember(t, db)
	valid := manager.UpdateTarget{FullName: "Ns-Only", Source: "thunderstore", Version: "2.0.0"}

	if rec := postQueueUpdates(t, rt, member, []manager.UpdateTarget{valid}); rec.Code != http.StatusForbidden {
		t.Errorf("member without mods.manage = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	for name, targets := range map[string][]manager.UpdateTarget{
		"empty":     {},
		"not newer": {valid, {FullName: "Ns-Lib", Source: "thunderstore", Version: "1.0.0"}},
	} {
		if rec := postQueueUpdates(t, rt, admin, targets); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: queue updates = %d, want 422 (%s)", name, rec.Code, rec.Body)
		}
	}
	if queued := listQueue(t, rt, admin); len(queued) != 0 {
		t.Errorf("queue after refused requests = %+v, want empty", queued)
	}
}

// TestQueuedUpdatesRunAsOneJob asserts the updates queued on a running server install in one
// job once it stops: one archive of the world, every package moved, one audit entry, and the
// queue emptied.
func TestQueuedUpdatesRunAsOneJob(t *testing.T) {
	rt, db, admin, _ := updateWorld(t)
	installsBefore := countRows(t, db, `SELECT COUNT(*) FROM job_runs WHERE kind = 'mod_install'`)
	queueAllUpdates(t, rt, db, admin)

	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "succeeded" {
		t.Fatalf("queued update = %+v, want succeeded", got)
	}

	if n := countRows(t, db, `SELECT COUNT(*) FROM job_runs WHERE kind = 'mod_install'`); n != installsBefore+1 {
		t.Errorf("install jobs = %d, want %d: one for every queued update", n, installsBefore+1)
	}
	if got := preUpdateArchives(t, db); got != 1 {
		t.Errorf("pre_update archives = %d, want 1", got)
	}
	rows := installedRows(t, db)
	for name, want := range map[string]string{"Ns-Only": "2.0.0", "Ns-Other": "1.1.0", "Ns-Lib": "1.1.0"} {
		if got := rows[name].Version; got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if queued := listQueue(t, rt, admin); len(queued) != 0 {
		t.Errorf("queue = %+v, want empty", queued)
	}
	if entries := modAuditEntries(t, db, "instances.mods.update"); len(entries) != 1 || entries[0].UserID != admin.ID {
		t.Errorf("update audit entries = %+v, want one for the queuing user", entries)
	}
}

// TestAQueuedNewInstallRunsAloneBeforeTheUpdates asserts a head that is not an update runs as
// its own install, leaving the queued updates for the next pass.
func TestAQueuedNewInstallRunsAloneBeforeTheUpdates(t *testing.T) {
	rt, db, admin, _ := updateWorld(t)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	if rec := postQueue(t, rt, admin, "Ns-New", "1.0.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}
	queueAllUpdates(t, rt, db, admin)

	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "succeeded" {
		t.Fatalf("queued install = %+v, want succeeded", got)
	}
	if got := installedRows(t, db)["Ns-New"].Version; got != "1.0.0" {
		t.Errorf("Ns-New = %q, want installed by the head", got)
	}
	if queued := listQueue(t, rt, admin); len(queued) != 3 {
		t.Errorf("queue = %+v, want the three updates left", queued)
	}
}

// TestQueuedUpdatesOfAnotherRequesterDoNotJoinTheBatch asserts entries queued by two users are
// never merged into one job credited to either of them.
func TestQueuedUpdatesOfAnotherRequesterDoNotJoinTheBatch(t *testing.T) {
	rt, db, admin, _ := updateWorld(t)
	grantModsManage(t, db)
	member := loadMember(t, db)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	if rec := postQueueUpdates(t, rt, member, []manager.UpdateTarget{
		{FullName: "Ns-Only", Source: "thunderstore", Version: "2.0.0"},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("member queue = %d (%s)", rec.Code, rec.Body)
	}
	if rec := postQueueUpdates(t, rt, admin, []manager.UpdateTarget{
		{FullName: "Ns-Lib", Source: "thunderstore", Version: "1.1.0"},
		{FullName: "Ns-Other", Source: "thunderstore", Version: "1.1.0"},
	}); rec.Code != http.StatusCreated {
		t.Fatalf("admin queue = %d (%s)", rec.Code, rec.Body)
	}

	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "succeeded" {
		t.Fatalf("first queued job = %+v, want succeeded", got)
	}
	queued := listQueue(t, rt, admin)
	if len(queued) != 2 || queued[0].FullName != "Ns-Lib" || queued[1].FullName != "Ns-Other" {
		t.Errorf("queue = %+v, want the admin's two updates left", queued)
	}
}

// TestTheQueueAnnouncesEachSubmissionAndItsEnd asserts the queue tells clients when it submits
// a job and once more when that job has finished with nothing after it.
func TestTheQueueAnnouncesEachSubmissionAndItsEnd(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	queue := rt.instances.ctl.Supervisor.ModQueue
	var announced []string
	queue.PublishMods = func(id string) { announced = append(announced, id) }
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	if rec := postQueue(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}

	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	queue.Drain(t.Context())
	if len(announced) != 1 {
		t.Fatalf("announcements after the submit = %v, want one", announced)
	}
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status != "succeeded" {
		t.Fatalf("queued install = %+v, want succeeded", got)
	}
	queue.Drain(t.Context())
	if len(announced) != 2 || announced[1] != "inst-a" {
		t.Errorf("announcements = %v, want a second one for inst-a once the job finished", announced)
	}
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

// TestAQueuedInstallIsDroppedWhenItsRequesterLosesAccess asserts the queue checks the
// queuing user's authority again when it runs: a user disabled in the meantime installs
// nothing, and the entry leaves the queue.
func TestAQueuedInstallIsDroppedWhenItsRequesterLosesAccess(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)
	grantModsManage(t, db)
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = 'inst-a'`)
	if rec := postQueue(t, rt, member, "OdinPlus-OdinArchitect", "1.7.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}

	seed(t, db, `UPDATE users SET disabled = 1 WHERE id = ?`, member.ID)
	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = 'inst-a'`)
	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())

	if n := countRows(t, db, `SELECT COUNT(*) FROM job_runs WHERE kind = 'mod_install'`); n != 0 {
		t.Errorf("%d install jobs ran for a disabled user, want none", n)
	}
	if queued := listQueue(t, rt, admin); len(queued) != 0 {
		t.Errorf("queue = %+v, want the entry dropped", queued)
	}
}

// TestAPlainStopAppliesEverythingWaitingForTheServerToGoDown asserts a stop, with no restart,
// settles config edits saved while the server ran and runs the queued installs, and starts
// nothing.
func TestAPlainStopAppliesEverythingWaitingForTheServerToGoDown(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, instanceStateRunning)
	savesOnStop(fake)
	path := seedConfigFile(t, rt)
	if rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, configURL("/"+seededConfigFile),
		jsonBody(t, map[string]any{"General.Enabled": false}))); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s)", rec.Code, rec.Body)
	}
	if rec := postQueue(t, rt, admin, "Ns-NotIndexed", "1.0.0"); rec.Code != http.StatusCreated {
		t.Fatalf("queue: status = %d (%s)", rec.Code, rec.Body)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/stop", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop = %d (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "succeeded" {
		t.Fatalf("stop = %+v, want succeeded", got)
	}
	if _, err := os.Stat(path + modconfig.PendingSuffix); !os.IsNotExist(err) {
		t.Errorf("pending config copy after the stop: %v, want settled", err)
	}

	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())
	if got := waitJob(t, rt, admin, lastJobOfKind(t, db, "mod_install")); got.Status == "" {
		t.Fatalf("queued install = %+v, want it run", got)
	}
	rt.instances.ctl.Supervisor.ModQueue.Drain(t.Context())
	if queued := listQueue(t, rt, admin); len(queued) != 0 {
		t.Errorf("queue = %+v, want empty", queued)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM job_runs WHERE kind = 'start'`); n != 0 {
		t.Errorf("%d start jobs after a plain stop, want none", n)
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
