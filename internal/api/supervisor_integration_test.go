//go:build integration

// Proves reconciliation against a real daemon: the label join that 08 §6.1 specifies, the
// panel-side guards of 08 §6 against `unless-stopped`, and the orphan surfacing that makes
// io.valmin.managed worth carrying.
//
// These use the real valheim-stub image and real containers, because the two facts under
// test — that Docker's labels come back the way they went in, and that a stopped container
// really is stopped afterwards — are facts about Docker, not about the panel. A fake proves
// neither.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// TestReconcileFindsARunningContainerByLabelAlone is 08 §6.1 steps 1 and 2, and the reason
// A2's labels are set-once: the panel joins Docker to the DB on io.valmin.instance.id, so a
// container_id the database has lost — or never had — is recoverable.
func TestReconcileFindsARunningContainerByLabelAlone(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, rt, db, d, "e2e-reconcile-found")

	// Start it behind the panel's back, exactly as `unless-stopped` would after a host
	// reboot, and blank the column so only the label can find it.
	var containerID string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = ?`, id).Scan(&containerID); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context(), containerID); err != nil {
		t.Fatalf("start container: %v", err)
	}
	seed(t, db, `UPDATE instances SET container_id = NULL WHERE id = ?`, id)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if got := instanceState(t, rt, admin, id); got != "running" {
		t.Errorf("state = %s, want running — Docker wins (08 §6.1)", got)
	}
	var repointed *string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = ?`, id).Scan(&repointed); err != nil {
		t.Fatal(err)
	}
	if repointed == nil || *repointed != containerID {
		t.Errorf("container_id = %v, want %s recovered from the label", repointed, containerID)
	}
}

// TestReconcileRecordsAContainerThatExitedOnItsOwn is 12 §2.2's observation row: the panel
// was down when the server stopped, and reconciliation is how it finds out.
func TestReconcileRecordsAContainerThatExitedOnItsOwn(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, rt, db, d, "e2e-reconcile-exited", "STUB_MODE=exit-early")
	seed(t, db, `UPDATE instances SET state = 'running' WHERE id = ?`, id)

	var containerID string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = ?`, id).Scan(&containerID); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context(), containerID); err != nil {
		t.Fatalf("start container: %v", err)
	}
	if _, err := d.Wait(t.Context(), containerID); err != nil {
		t.Fatalf("wait for the stub to exit: %v", err)
	}

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Errorf("state = %s, want stopped", got)
	}
}

// TestReconcileParksAnInterruptedStartThatNeverRan is 12 §9.2's `starting` + not-running row.
// The daemon died between claiming the start and the container coming up, and the honest
// answer is that the start simply did not happen.
func TestReconcileParksAnInterruptedStartThatNeverRan(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, rt, db, d, "e2e-reconcile-starting")
	seed(t, db, `UPDATE instances SET state = 'starting' WHERE id = ?`, id)

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := instanceState(t, rt, admin, id); got != "stopped" {
		t.Errorf("state = %s, want stopped", got)
	}
}

// TestReconcileReestablishesReadinessForARunningStart is the other `starting` row, and it is
// the one that proves ADR-043 survives a crash: the stub's real readiness line is already in
// Docker's log, so the instance resolves to `running` off the log rather than off a fresh
// fifteen-second settle.
func TestReconcileReestablishesReadinessForARunningStart(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	id := seedRealInstance(t, rt, db, d, "e2e-reconcile-ready")

	var containerID string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT container_id FROM instances WHERE id = ?`, id).Scan(&containerID); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(t.Context(), containerID); err != nil {
		t.Fatalf("start container: %v", err)
	}
	waitForReadinessLine(t, d, containerID)
	seed(t, db, `UPDATE instances SET state = 'starting' WHERE id = ?`, id)

	start := time.Now()
	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := instanceState(t, rt, admin, id); got != "running" {
		t.Errorf("state = %s, want running", got)
	}
	// Settle is deliberately zero on this path: the line is already in the log, and waiting
	// jobs.ready_settle per instance would stall the daemon's own startup for no answer.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("recovery took %s — it waited out a settle window it did not need", elapsed)
	}
}

// TestOrphanedContainerSurvivesReconciliation is 08 §6.1's last bullet against a real
// daemon. A container the panel made, whose row is gone, is reported for adoption — never
// removed, because removing it would destroy a running server to tidy a table.
func TestOrphanedContainerSurvivesReconciliation(t *testing.T) {
	rt, _, d, admin := lifecycleRouter(t)
	containerID, err := d.Create(t.Context(), &runtime.ContainerSpec{
		User:  testContainerUser,
		Name:  "valmin-e2e-orphan-" + store.NewID()[:6],
		Image: integrationGameImage, Labels: instance.Labels("e2e-orphan", 2471),
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), containerID, true) })

	if err := rt.Supervisor().Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := d.Inspect(t.Context(), containerID); err != nil {
		t.Fatalf("the orphaned container was removed: %v", err)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/instances/orphans", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /instances/orphans = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []Orphan `json:"items"`
	}
	decodeInto(t, rec, &page)
	for _, o := range page.Items {
		if o.InstanceID == "e2e-orphan" && o.BasePort == 2471 {
			return
		}
	}
	t.Errorf("orphans = %+v, want one for e2e-orphan on 2471", page.Items)
}

// waitForReadinessLine blocks until the stub has actually printed 03 §3.5's literal, so the
// test asserts against a log that really contains it rather than racing the container.
func waitForReadinessLine(t *testing.T, d *runtime.Docker, containerID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ready, err := instance.AwaitReady(t.Context(), d, containerID, 0, time.Second)
		if err == nil && ready {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s never printed the readiness line", containerID)
}
