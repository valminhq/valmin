package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// recordingRuntime notes the order in which the supervisor reaches Docker. 12 §9.1's step 2
// before step 3 (C6) is an ordering claim, and an ordering claim is only proved by watching
// the calls happen.
type recordingRuntime struct {
	*runtime.Fake
	calls *[]string
}

func (r recordingRuntime) List(ctx context.Context, labels map[string]string) ([]runtime.Container, error) {
	*r.calls = append(*r.calls, "list")
	return r.Fake.List(ctx, labels)
}

func (r recordingRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	*r.calls = append(*r.calls, "inspect")
	return r.Fake.Inspect(ctx, id)
}

// supervisorWorld is lifecycleWorld's shape with the runtime wrapped so calls are recorded,
// and the supervisor's own owner string held so a test can plant a job under a *different*
// owner — which is what "a process that no longer exists" means to the sweep.
func supervisorWorld(t *testing.T) (rt *Router, db *store.DB, fake *runtime.Fake, calls *[]string) {
	t.Helper()
	rt, db, fake, _, _ = lifecycleWorld(t)
	recorded := []string{}
	inst := rt.Supervisor().inst
	inst.Runtime = recordingRuntime{Fake: fake, calls: &recorded}
	return rt, db, fake, &recorded
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

// stateOf reads the seeded instance's state — the observer's only visible output.
func stateOf(t *testing.T, db *store.DB) string {
	t.Helper()
	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = ?`, staleJobInstance).Scan(&state); err != nil {
		t.Fatalf("read state of %s: %v", staleJobInstance, err)
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

// TestRecoverSweepsDeadJobsBeforeInspectingAnyContainer is C6, and it is asserted by call
// ordering rather than by reading the code: a sweep that runs second leaves the reconciler
// meeting an instance whose lock is held by a process that no longer exists.
func TestRecoverSweepsDeadJobsBeforeInspectingAnyContainer(t *testing.T) {
	rt, db, fake, calls := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "provisioning")
	jobID := seedStaleJob(t, db, "provision", "", "{}")

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The sweep touches no container at all, so the first Docker call of the whole startup
	// must be reconciliation's own labelled List.
	if len(*calls) == 0 || (*calls)[0] != "list" {
		t.Errorf("first runtime call = %v, want the reconcile pass's list", *calls)
	}
	swept := jobRow(t, db, jobID)
	if swept.Status != "failed" || swept.ErrorCode == nil || *swept.ErrorCode != "interrupted" {
		t.Errorf("swept job = %s/%v, want failed/interrupted", swept.Status, swept.ErrorCode)
	}
	if swept.LeaseOwner != nil {
		t.Errorf("swept job still holds a lease: %v", *swept.LeaseOwner)
	}
	// The lock is released, or nothing could ever run against this instance again.
	held, err := db.HeldLockKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if held[jobs.InstanceLockKey("inst-a")] {
		t.Error("the swept job's lock was not released")
	}
}

// TestRecoverParksAnUncheckpointedProvisionInError is 12 §9.2's `provisioning` row, negative
// half: no checkpoint, no resume. A provision that died before proving it could make any
// progress at all must not be retried on every boot.
func TestRecoverParksAnUncheckpointedProvisionInError(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "provisioning")
	seedStaleJob(t, db, "provision", "", "{}")

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %s, want error", got)
	}
}

// TestRecoverResumesACheckpointedProvision is the positive half: a checkpoint is the
// permission to resume, and the resumed run is a real job holding the instance's lock again.
func TestRecoverResumesACheckpointedProvision(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "provisioning")
	dead := seedStaleJob(t, db, "provision", "dirs_created", `{"start_after_provision":false}`)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var resumed int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'provision' AND id <> ?`, dead).Scan(&resumed); err != nil {
		t.Fatal(err)
	}
	if resumed != 1 {
		t.Fatalf("resumed provision jobs = %d, want 1", resumed)
	}
	// The resumed job is the panel's own doing, not the user's who clicked Create days ago.
	var requestedBy *string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT requested_by FROM job_runs WHERE kind = 'provision' AND id <> ?`, dead).Scan(&requestedBy); err != nil {
		t.Fatal(err)
	}
	if requestedBy != nil {
		t.Errorf("resumed job attributed to %q, want nobody", *requestedBy)
	}
}

// TestRecoverRerunsAnInterruptedDelete is 12 §9.2's `deleting` row, and 12 §10's guard with
// it: the re-run carries the dead job's own keep_worlds rather than a fresh default.
func TestRecoverRerunsAnInterruptedDelete(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "deleting")
	seedStaleJob(t, db, "delete", "", `{"keep_worlds":false}`)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.Reader.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM instances WHERE id = 'inst-a'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the interrupted delete was never re-run to completion")
}

// TestRecoverResolvesAnInterruptedStop is 12 §9.2's two `stopping` rows in one test: the
// container is gone, so the stop demonstrably finished.
func TestRecoverResolvesAnInterruptedStop(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopping")
	seedStaleJob(t, db, "stop", "", "{}")
	fake.Get(containerID).Exit(0)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %s, want stopped", got)
	}
}

// TestRecoverParksAStopThatNeverHappened is the other `stopping` row: the container is still
// up, so a stop was requested and demonstrably did not complete.
func TestRecoverParksAStopThatNeverHappened(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopping")
	seedStaleJob(t, db, "stop", "", "{}")
	if err := fake.Start(t.Context(), containerID); err != nil {
		t.Fatal(err)
	}

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %s, want error", got)
	}
}

// TestObserverIsSilentWhileALockIsHeld is C14, which 12 §1 calls the single most likely
// concurrency bug in the daemon: during a job the container will exit *because* the job
// stopped it, and an observer that writes on that event races the job that caused it.
func TestObserverIsSilentWhileALockIsHeld(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")
	// A live lock, held by this very process — a job in flight, not a dead one.
	seed(t, db, `INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES (?, ?, ?)`,
		jobs.InstanceLockKey("inst-a"), store.NewID(), store.Now())

	fake.Get(containerID).Exit(0)
	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %s, want running — the observer wrote while a lock was held", got)
	}

	// Release the lock and the very same exit is now the observer's to record.
	seed(t, db, `DELETE FROM job_locks WHERE lock_key = ?`, jobs.InstanceLockKey("inst-a"))
	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := stateOf(t, db); got != "stopped" {
		t.Errorf("state = %s, want stopped once the lock was free", got)
	}
}

// TestObserverParksAnOOMKilledContainerAndStopsIt is 08 §6's first guard. An OOM-kill is a
// SIGKILL and therefore probable world damage (03 §3.3); `unless-stopped` would restart it
// into the same limit and corrupt the world again on a timer.
func TestObserverParksAnOOMKilledContainerAndStopsIt(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "running")
	fake.Get(containerID).OOMKill()

	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %s, want error", got)
	}
	if fake.Get(containerID).Running {
		t.Error("the container is running again: unless-stopped resurrected what the panel parked")
	}
}

// TestObserverRecordsAContainerStartedOutsideThePanel is 08 §6.1's "DB stopped, container
// running → running. Docker wins".
func TestObserverRecordsAContainerStartedOutsideThePanel(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "stopped")
	if err := fake.Start(t.Context(), containerID); err != nil {
		t.Fatal(err)
	}

	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := stateOf(t, db); got != "running" {
		t.Errorf("state = %s, want running — Docker wins", got)
	}
}

// TestObserverNeverMovesAnInstanceParkedInError is 12 §2.4: `error` has exactly one exit and
// it is a human pressing acknowledge. An observer that helpfully un-parked it would undo the
// one thing the state is for.
func TestObserverNeverMovesAnInstanceParkedInError(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, rt, db, fake, "error")
	if err := fake.Start(t.Context(), containerID); err != nil {
		t.Fatal(err)
	}

	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := stateOf(t, db); got != "error" {
		t.Errorf("state = %s, want error", got)
	}
}

// TestReconcileRepointsAStaleContainerID is what makes the label join load-bearing: the
// panel finds its containers by io.valmin.instance.id, so a container_id column that no
// longer matches is the stale side and gets corrected rather than believed.
func TestReconcileRepointsAStaleContainerID(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	live, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Name: "inst-a", Labels: instance.Labels("inst-a", 2456),
	})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instances SET container_id = 'a-container-that-is-gone' WHERE id = 'inst-a'`)

	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = 'inst-a'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != live {
		t.Errorf("container_id = %s, want %s — the label join found the truth", got, live)
	}
}

// TestOrphanedContainerIsReportedNotRemoved is 08 §6.1's last bullet, and the reason
// io.valmin.managed is worth carrying: a container whose row is gone is surfaced for
// adoption, never deleted. Removing it would destroy a running server to tidy a table.
func TestOrphanedContainerIsReportedNotRemoved(t *testing.T) {
	rt, _, fake, _ := supervisorWorld(t)
	orphan, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Name: "valmin-orphan", Labels: instance.Labels("inst-gone", 2461),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}

	if err := rt.Supervisor().reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if fake.Get(orphan) == nil {
		t.Fatal("the orphaned container was removed")
	}

	found, err := rt.Supervisor().Orphans(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].InstanceID != "inst-gone" || found[0].BasePort != 2461 {
		t.Fatalf("orphans = %+v, want one for inst-gone on 2461", found)
	}
	if !found[0].Running {
		t.Error("the orphan is running and should be reported as such")
	}
}

// TestReconcileFindsEveryContainerAfterTheDatabaseIsLost is the label join taken to its
// limit: with no rows at all, every managed container is an orphan and every one is found.
func TestReconcileFindsEveryContainerAfterTheDatabaseIsLost(t *testing.T) {
	rt, _, fake, _ := supervisorWorld(t)
	for _, id := range []string{"inst-1", "inst-2", "inst-3"} {
		c, err := fake.Create(t.Context(), &runtime.ContainerSpec{Labels: instance.Labels(id, 2456)})
		if err != nil {
			t.Fatal(err)
		}
		if err := fake.Start(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}

	found, err := rt.Supervisor().Orphans(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 3 {
		t.Errorf("found %d containers by label, want 3", len(found))
	}
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

// TestResumeIntentIsHonouredOnlyForWorldSafeKinds is ADR-032. No M1 kind sets resume_after,
// so the row is planted directly — the branch that is never written is the branch that is
// wrong at M4.
func TestResumeIntentIsHonouredOnlyForWorldSafeKinds(t *testing.T) {
	rt, db, fake, _ := supervisorWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	id := seedStaleJob(t, db, "restore", "", "{}")
	seed(t, db, `UPDATE job_runs SET resume_after = TRUE WHERE id = ?`, id)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var started int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'start'`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started != 0 {
		t.Error("a restore's resume intent was honoured — B7 forbids auto-starting after one")
	}
}

// TestStartAfterProvisionSubmitsAStartOnceTheLockIsFree is 12 §2.2's "then a start job if
// the wizard asked for one", and the reason jobs.Outcome grew an AfterFinish hook: a job
// cannot claim its own lock key while it still holds it.
func TestStartAfterProvisionSubmitsAStartOnceTheLockIsFree(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	containerID := *inst.ContainerID

	// The provision runner itself needs a real SteamCMD; what is under test is the hook, so
	// the outcome that carries it is exercised directly.
	handlers := rt.Supervisor().inst
	after := handlers.afterProvision(&provisionRun{
		instanceID: "inst-a", name: "inst-a", startAfterProvision: true, requestedBy: admin.ID,
	}, containerID)
	if after == nil {
		t.Fatal("start_after_provision produced no hook")
	}
	after(t.Context())

	var started int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = 'start'`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("start jobs after provision = %d, want 1", started)
	}
	if handlers.afterProvision(&provisionRun{instanceID: "inst-a"}, containerID) != nil {
		t.Error("a provision the wizard did not ask to start still produced a hook")
	}
}
