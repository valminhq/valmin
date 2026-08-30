package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// waitJob polls GET /jobs/{id} until it reaches a terminal status, the same wait
// provision_integration_test.go's build-tagged version does — this one runs against the
// fake runtime, so it never needs a real Docker daemon.
func waitJob(t *testing.T, rt *Router, u *store.User, jobID string) jobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last jobView
	for time.Now().Before(deadline) {
		rec := as(rt, u, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, http.NoBody))
		decodeInto(t, rec, &last)
		switch last.Status {
		case "succeeded", "failed", "cancelled":
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal status in time, last = %+v", jobID, last)
	return jobView{}
}

// lifecycleWorld is world()'s shape, but it hands the test the *runtime.Fake behind the
// router so containers can be scripted, and it seeds one instance with a real fake
// container already attached — every lifecycle handler requires one.
func lifecycleWorld(t *testing.T) (rt *Router, db *store.DB, fake *runtime.Fake, admin, member *store.User) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir
	// ADR-043's fallback only fires once ready_settle elapses; the default (15s) would make
	// every start/restart test here wait it out for real.
	cfg.Jobs.ReadySettle = config.Duration(20 * time.Millisecond)
	cfg.Jobs.ReadyTimeout = config.Duration(2 * time.Second)

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)

	fake = runtime.NewFake()
	rt, err = NewRouter(&cfg, h.DB, h, k, false, testEngine(h.DB, &cfg), fake)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, u := range []struct {
		id, name string
		role     store.Role
	}{{"u-admin", "ada", store.RoleAdmin}, {"u-member", "mel", store.RoleMember}} {
		seed(t, h.DB, `INSERT INTO users (id, username, password_hash, role, created_at)
			VALUES (?, ?, 'argon2id$stub', ?, ?)`, u.id, u.name, string(u.role), store.Now())
	}
	return rt, h.DB, fake,
		&store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin},
		&store.User{ID: "u-member", Username: "mel", Role: store.RoleMember}
}

// seedInstance inserts inst-a in state with a real fake container attached, returning the
// container's id so a test can script it.
func seedInstance(t *testing.T, db *store.DB, fake *runtime.Fake, state string) string {
	t.Helper()
	containerID, err := fake.Create(t.Context(), &runtime.ContainerSpec{Name: "inst-a"})
	if err != nil {
		t.Fatal(err)
	}
	if state == string(instanceStateRunning) {
		if err := fake.Start(t.Context(), containerID); err != nil {
			t.Fatal(err)
		}
	}
	seed(t, db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', ?, ?, ?, 2456, 'Server', 'World', 'v1.k.n.ct', 'cp-inst-a', ?, ?)`,
		state, containerID, t.TempDir(), store.Now(), store.Now())
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-member', 'inst-a', 'viewer', '[]', ?)`, store.Now())
	return containerID
}

const instanceStateRunning = "running"

// TestStartMovesStoppedToRunning is 12 §2.2's start row, exercised end to end: the fake
// container never emits the readiness line, so this also proves ADR-043's fallback lands
// the instance in `running` rather than `error`.
func TestStartMovesStoppedToRunning(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, db, fake, "stopped")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}

	var stub jobView
	decodeInto(t, rec, &stub)
	waitJob(t, rt, admin, stub.JobID)

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Errorf("state = %q, want running", state)
	}
}

// TestStartFromErrorIsRejected is 12 §2.4: a parked instance has probable world damage, and
// retrying a start automatically would corrupt it again.
func TestStartFromErrorIsRejected(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, db, fake, "error")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "invalid_state" {
		t.Errorf("code = %q, want invalid_state", got)
	}
}

// TestStartRequiresTheAction: a viewer can see the instance but not start it (F3, D1).
func TestStartRequiresTheAction(t *testing.T) {
	rt, db, fake, _, member := lifecycleWorld(t)
	seedInstance(t, db, fake, "stopped")

	rec := as(rt, member, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestStartGoesToErrorWhenTheContainerExits is 12 §2.2's other start row: only the
// container exiting inside the readiness window is a real failure.
func TestStartGoesToErrorWhenTheContainerExits(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, db, fake, "stopped")
	fake.OnStart = func(c *runtime.FakeContainer) { c.Exit(1) }

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "failed" {
		t.Fatalf("job status = %q, want failed", final.Status)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
}

// TestRestartStopsThenStarts is 12 §3.1's `stopping`→`starting` row: one job, one lock,
// ending in `running`.
func TestRestartStopsThenStarts(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, db, fake, "running")
	fake.Get(containerID).Stdout("World save writing finished\n")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/restart", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)
	if final.Status != "succeeded" {
		t.Fatalf("job status = %q, want succeeded (%+v)", final.Status, final)
	}
	if final.Clean == nil || !*final.Clean {
		t.Errorf("clean = %v, want true: the save line was written before the restart", final.Clean)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Errorf("state = %q, want running", state)
	}
	if !fake.Get(containerID).Running {
		t.Error("the fake container was never restarted")
	}
}

// TestStopRecordsCleanFalseOnTheFullLiteral is B2 at this call site: `finishing` must not
// satisfy the save-complete pattern.
func TestStopRecordsCleanFalseOnTheFullLiteral(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, db, fake, "running")
	fake.Get(containerID).Stdout("World save writing finishing\n")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/stop", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)

	if final.Clean == nil || *final.Clean {
		t.Errorf("clean = %v, want false: the log never carried the full literal", final.Clean)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" {
		t.Errorf("state = %q, want stopped even though the save was not confirmed", state)
	}
}

// TestStopRecordsCleanTrueOnTheFullLiteral is the positive half of the same test.
func TestStopRecordsCleanTrueOnTheFullLiteral(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	containerID := seedInstance(t, db, fake, "running")
	fake.Get(containerID).Stdout("World save writing finished\n")

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/stop", http.NoBody))
	var stub jobView
	decodeInto(t, rec, &stub)
	final := waitJob(t, rt, admin, stub.JobID)

	if final.Clean == nil || !*final.Clean {
		t.Errorf("clean = %v, want true", final.Clean)
	}
}

// TestDeleteWithDefaultsKeepsWorlds is 12 §10: keep_worlds defaults to true, and the panel
// never removes worlds/ outside an explicit keep_worlds=false.
func TestDeleteWithDefaultsKeepsWorlds(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, db, fake, "stopped")

	var dataDir string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT data_dir FROM instances WHERE id = 'inst-a'`).Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	worldsDir := dataDir + "/worlds"
	if err := os.MkdirAll(worldsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodDelete, "/api/v1/instances/inst-a", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	waitJob(t, rt, admin, stub.JobID)

	if !dirExists(t, worldsDir) {
		t.Error("worlds/ was removed despite keep_worlds defaulting to true")
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM instances WHERE id = 'inst-a'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("instance row still exists after a succeeded delete")
	}
}

// TestDeleteWithKeepWorldsFalseRemovesEverything is the explicit opt-out.
func TestDeleteWithKeepWorldsFalseRemovesEverything(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, db, fake, "stopped")

	var dataDir string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT data_dir FROM instances WHERE id = 'inst-a'`).Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	worldsDir := dataDir + "/worlds"
	if err := os.MkdirAll(worldsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodDelete, "/api/v1/instances/inst-a?keep_worlds=false", http.NoBody))
	var stub jobView
	decodeInto(t, rec, &stub)
	waitJob(t, rt, admin, stub.JobID)

	if dirExists(t, worldsDir) {
		t.Error("worlds/ survived keep_worlds=false")
	}
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}
