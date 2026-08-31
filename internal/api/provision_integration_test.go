//go:build integration

// Proves the provision job end to end against a real daemon: POST /instances, a real
// SteamCMD run (the stub image, never Steam), a real `cp -a --reflink=auto` clone, and —
// only inside a uid-10000 container, matching production (08 §2) — a real container
// creation. Everywhere else this process runs as some other uid, which is exactly the
// case A4's ownership check exists to catch, so this test asserts whichever outcome the
// environment actually supports rather than assuming one.
package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	integrationGameImage     = "valmin/valheim-stub:dev"
	integrationSteamCMDImage = "valmin/steamcmd-stub:dev"
)

func waitForJobTerminal(t *testing.T, rt *Router, admin *store.User, jobID string) jobView {
	t.Helper()
	// `↯` Every 500 ms, not every 100 ms. The chain's per-IP limiter is 300 requests a
	// minute (11 §7) and this loop used to spend the entire budget in thirty seconds, so a
	// job that took a little longer than expected made the *poller* fail with a 429 that
	// decoded into a jobView as gibberish. Found when Q31's retry made one provision slower.
	deadline := time.Now().Add(90 * time.Second)
	var last jobView
	for time.Now().Before(deadline) {
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("polling job %s: status %d (%s)", jobID, rec.Code, rec.Body)
		}
		decodeInto(t, rec, &last)
		switch last.Status {
		case "succeeded", "failed", "cancelled":
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach a terminal status in time, last = %+v", jobID, last)
	return jobView{}
}

// TestCreateInstanceProvisionsEndToEnd is WP-M1-13's capstone.
func TestCreateInstanceProvisionsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.ExternalURL = testOrigin
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir
	cfg.Game.Image = integrationGameImage
	cfg.Game.SteamCMDImage = integrationSteamCMDImage
	cfg.Jobs.ProgressInterval = config.Duration(10 * time.Millisecond)

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
	admin := &store.User{ID: "u-admin", Username: "ada", Role: store.RoleAdmin}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances", jsonBody(t, validCreateBody("e2e-provision"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if stub.InstanceID == nil {
		t.Fatal("job stub carries no instance_id")
	}
	instanceID := *stub.InstanceID

	final := waitForJobTerminal(t, rt, admin, stub.JobID)

	instRec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/"+instanceID, http.NoBody))
	var inst store.Instance
	decodeInto(t, instRec, &inst)

	if os.Getuid() == instance.WantCloneUID {
		// Only true inside a uid-10000 container, matching production (08 §2): the full
		// happy path, all the way through a real container.
		assertProvisionSucceeded(t, rt, admin, d, stub.JobID, instanceID, &final, &inst)
		return
	}

	// The common case for a dev/CI host: this test process is not uid 10000, so A4's
	// ownership check must catch the mismatch and fail the job loudly — never silently
	// accept it, and never chown to repair it.
	if final.Status != "failed" {
		t.Fatalf("job = %+v, want failed (a uid mismatch went uncaught)", final)
	}
	if inst.State != "error" {
		t.Errorf("instance state = %q, want error", inst.State)
	}
	if inst.ContainerID != nil {
		t.Error("a job that failed before container creation must not have a container_id")
	}
}

func assertProvisionSucceeded(
	t *testing.T, rt *Router, admin *store.User, d *runtime.Docker,
	jobID, instanceID string, final *jobView, inst *store.Instance,
) {
	t.Helper()
	if final.Status != "succeeded" {
		t.Fatalf("job = %+v, want succeeded\n%s", final, jobLog(t, rt, admin, jobID))
	}
	if inst.State != "stopped" {
		t.Errorf("instance state = %q, want stopped", inst.State)
	}
	if inst.ContainerID == nil {
		t.Fatal("instance has no container_id after a succeeded provision")
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), *inst.ContainerID, true) })

	c, err := d.Inspect(t.Context(), *inst.ContainerID)
	if err != nil {
		t.Fatalf("inspect created container: %v", err)
	}
	if c.Labels["io.valmin.instance.id"] != instanceID {
		t.Errorf("labels = %v, missing the instance id", c.Labels)
	}
	if c.Running {
		t.Error("provision must not start the container — that is the start job's")
	}
}

func jobLog(t *testing.T, rt *Router, admin *store.User, jobID string) string {
	t.Helper()
	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID, http.NoBody))
	return rec.Body.String()
}
