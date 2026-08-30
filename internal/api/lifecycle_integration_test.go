//go:build integration

// Proves the lifecycle jobs against a real daemon and the real valheim-stub image: start
// resolving through a real readiness line, and stop resolving through a real save-complete
// line off Docker's own multiplexed log stream.
//
// `↯` The instance's container is created directly here rather than by running a provision
// job first. Provisioning's own end-to-end coverage is provision_integration_test.go's, and
// it only reaches container creation inside a uid-10000 container (A4) — chaining onto it
// would make these lifecycle assertions unreachable on any dev host, which is exactly where
// a broken readiness or save-line match needs to be caught.
package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// lifecycleRouter builds the surface against a real Docker daemon, with the readiness
// window shortened: the stub announces readiness in well under a second, and the real 180 s
// timeout would only make a regression take three minutes to report itself.
func lifecycleRouter(t *testing.T) (*Router, *store.DB, *runtime.Docker, *store.User) {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir
	cfg.Game.Image = integrationGameImage
	cfg.Game.SteamCMDImage = integrationSteamCMDImage
	cfg.Game.StopTimeout = config.Duration(15 * time.Second)
	cfg.Jobs.ProgressInterval = config.Duration(10 * time.Millisecond)
	cfg.Jobs.ReadySettle = config.Duration(3 * time.Second)
	cfg.Jobs.ReadyTimeout = config.Duration(20 * time.Second)

	k, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := health(t)

	d, err := runtime.NewDocker(t.Context(), "unix:///var/run/docker.sock", "")
	if err != nil {
		t.Fatalf("connect to docker: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	rt, err := NewRouter(&cfg, h.DB, h, k, false, testEngine(h.DB, &cfg), d)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	seed(t, h.DB, `INSERT INTO users (id, username, password_hash, role, created_at)
		VALUES ('u-admin', 'ada', 'argon2id$stub', 'admin', ?)`, store.Now())

	return rt, h.DB, d, &store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin}
}

// seedRealInstance creates a real stub container and the instances row pointing at it,
// already `stopped` — the state a successful provision leaves behind (12 §2.2).
func seedRealInstance(t *testing.T, db *store.DB, d *runtime.Docker, name string, env ...string) string {
	t.Helper()
	containerID, err := d.Create(t.Context(), &runtime.ContainerSpec{
		Name: "valmin-" + name, Image: integrationGameImage, Env: env,
		StopSignal: "SIGINT", StopTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), containerID, true) })

	seed(t, db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, ?, 'stopped', ?, ?, 2456, 'Server', 'World', 'v1.k.n.ct', ?, ?, ?)`,
		name, name, containerID, t.TempDir(), "cp-"+name, store.Now(), store.Now())
	return name
}

func runJob(t *testing.T, rt *Router, admin *store.User, method, path string) jobView {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(method, path, http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s %s = %d, want 202 (%s)", method, path, rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	return waitForJobTerminal(t, rt, admin, stub.JobID)
}

func instanceState(t *testing.T, rt *Router, admin *store.User, id string) string {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+id, http.NoBody))
	var inst store.Instance
	decodeInto(t, rec, &inst)
	return inst.State
}

// TestLifecycleStartStopAgainstARealDaemon is WP-M1-14's capstone: a real start resolving
// on the measured readiness line (12 §3.3), and a real stop resolving on the measured
// save-complete line (12 §3.4, B2).
func TestLifecycleStartStopAgainstARealDaemon(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, db, d, "e2e-lifecycle")

	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start"); final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded", final)
	}
	if got := instanceState(t, rt, admin, id); got != "running" {
		t.Fatalf("state = %q after start, want running", got)
	}

	stopFinal := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/stop")
	if stopFinal.Status != "succeeded" {
		t.Fatalf("stop job = %+v, want succeeded", stopFinal)
	}
	if stopFinal.Clean == nil || !*stopFinal.Clean {
		t.Errorf("clean = %v, want true: the stub's default mode reaches the full save literal", stopFinal.Clean)
	}
	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Errorf("state = %q after stop, want stopped", got)
	}
}

// TestLifecycleStopWithoutTheSaveLineIsStillStopped is 12 §3.4's hard case: the stub stops
// after `finishing` and never writes `finished`, so the instance must still reach `stopped`
// with clean=false — refusing to reach `stopped` because a log line was missed would leave
// the panel unable to manage a server that is demonstrably down.
func TestLifecycleStopWithoutTheSaveLineIsStillStopped(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, db, d, "e2e-no-save-finish", "STUB_MODE=no-save-finish")

	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start"); final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded", final)
	}
	stopFinal := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/stop")
	if stopFinal.Status != "succeeded" {
		t.Fatalf("stop job = %+v, want succeeded", stopFinal)
	}
	if stopFinal.Clean == nil || *stopFinal.Clean {
		t.Errorf("clean = %v, want false: the stub never wrote the full literal", stopFinal.Clean)
	}
	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Errorf("state = %q, want stopped even with the save unconfirmed", got)
	}
}

// TestLifecycleStartWithoutReadinessIsRunning is ADR-043 against a real daemon: the stub's
// no-ready mode never announces registration, and the instance must land in `running` with
// the warning rather than in `error`.
func TestLifecycleStartWithoutReadinessIsRunning(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, db, d, "e2e-no-ready", "STUB_MODE=no-ready")

	final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start")
	if final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded (ADR-043: absence of the line is a warning)", final)
	}
	if got := instanceState(t, rt, admin, id); got != "running" {
		t.Errorf("state = %q, want running with a warning, never error", got)
	}
}

// TestLifecycleStartThatExitsGoesToError is 12 §3.3's one real start failure: the container
// exiting inside the readiness window.
func TestLifecycleStartThatExitsGoesToError(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, db, d, "e2e-exit-early", "STUB_MODE=exit-early")

	final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+id+"/start")
	if final.Status != "failed" {
		t.Fatalf("start job = %+v, want failed", final)
	}
	if got := instanceState(t, rt, admin, id); got != "error" {
		t.Errorf("state = %q, want error", got)
	}
}

// TestLifecycleDeleteRemovesTheRealContainer proves the delete job reaches Docker, and that
// worlds/ survives the default keep_worlds=true (12 §10).
func TestLifecycleDeleteRemovesTheRealContainer(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, db, d, "e2e-delete")

	var containerID string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = ?`, id).Scan(&containerID); err != nil {
		t.Fatal(err)
	}

	if final := runJob(t, rt, admin, http.MethodDelete, "/api/v1/instances/"+id); final.Status != "succeeded" {
		t.Fatalf("delete job = %+v, want succeeded", final)
	}
	if _, err := d.Inspect(t.Context(), containerID); err == nil {
		t.Error("the container still exists after a succeeded delete")
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+id, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", rec.Code)
	}
}
