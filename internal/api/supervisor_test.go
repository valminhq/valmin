package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// supervisorWorld is lifecycleWorld without the users, for tests that drive the supervisor.
func supervisorWorld(t *testing.T) (rt *Server, db *store.DB, fake *runtime.Fake) {
	t.Helper()
	rt, db, fake, _, _ = lifecycleWorld(t)
	return rt, db, fake
}

// staleJobInstance is the instance seedInstance always creates; the stale jobs planted
// against it are all instance-scoped, so they all share its lock key.
const staleJobInstance = "inst-a"

// seedStaleJob plants a `running` job row owned by a boot id that is not this process's —
// the exact shape 12 §9.1 step 2 defines as belonging to a dead process — and takes its lock.
func seedStaleJob(t *testing.T, db *store.DB, kind, checkpoint, payload string) string {
	t.Helper()
	id := store.NewID()
	var cp any
	if checkpoint != "" {
		cp = checkpoint
	}
	seed(t, db, `INSERT INTO job_runs (
		id, kind, status, lock_key, instance_id, instance_name, payload, checkpoint,
		lease_owner, lease_until, created_at, started_at
	) VALUES (?, ?, 'running', ?, ?, 'inst-a', ?, ?, 'panel:a-boot-that-died', ?, ?, ?)`,
		id, kind, jobs.InstanceLockKey(staleJobInstance), staleJobInstance, payload, cp,
		store.FormatTime(time.Now().Add(30*time.Second)), store.Now(), store.Now())
	seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		jobs.InstanceLockKey(staleJobInstance), id, store.Now())
	return id
}

// stateOf reads the seeded instance's state — the observer's only visible output, and what a
// backup or restart job is judged by once its own job row says succeeded.
func stateOf(t *testing.T, db *store.DB) string {
	t.Helper()
	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = ?`, seededInstanceID).Scan(&state); err != nil {
		t.Fatalf("read state of %s: %v", seededInstanceID, err)
	}
	return state
}

func jobRow(t *testing.T, db *store.DB, id string) *store.Job {
	t.Helper()
	j, err := db.JobByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatalf("job %s is gone", id)
	}
	return j
}

// TestOrphansEndpointIsAdminOnly is D15: an orphan listing is a panel-wide fact carrying
// container ids and ports, and there is no grant that could scope it.
func TestOrphansEndpointIsAdminOnly(t *testing.T) {
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	if rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/orphans", http.NoBody)); rec.Code != http.StatusForbidden {
		t.Errorf("member = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/orphans", http.NoBody)); rec.Code != http.StatusOK {
		t.Errorf("admin = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// TestStartAfterProvisionSubmitsAStartOnceTheLockIsFree is 12 §2.2's "then a start job if
// the wizard asked for one", and the reason jobs.Outcome grew an AfterFinish hook: a job
// cannot claim its own lock key while it still holds it.
func TestStartAfterProvisionSubmitsAStartOnceTheLockIsFree(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	// The provision runner itself needs a real SteamCMD; what is under test is the chain, so
	// the operation the finished provision leaves behind is advanced directly.
	handlers := rt.instances
	if err := handlers.ctl.Operations.Create(
		t.Context(), "inst-a", control.OperationCreate, admin.ID, &control.OperationPlan{Start: true}); err != nil {
		t.Fatal(err)
	}
	finishStep(t, handlers, db, t.Context(), "inst-a", jobs.KindProvision, control.ProvisionPayload{})
	handlers.ctl.Operations.Advance(t.Context(), "inst-a")

	var started int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'start'`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("start jobs after provision = %d, want 1", started)
	}
}
