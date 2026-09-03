package api

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// currentContainer resolves the container the instance is presently pointed at. A rebuild
// changes that id, so a test must not hold one captured before starting.
func currentContainer(t *testing.T, db *store.DB, fake *runtime.Fake) *runtime.FakeContainer {
	t.Helper()
	const instanceID = seededInstanceID
	inst, err := db.InstanceByID(t.Context(), instanceID)
	if err != nil {
		t.Fatalf("load instance %s: %v", instanceID, err)
	}
	if inst == nil || inst.ContainerID == nil {
		t.Fatalf("instance %s has no container", instanceID)
	}
	c := fake.Get(*inst.ContainerID)
	if c == nil {
		t.Fatalf("instance %s points at container %s, which does not exist", instanceID, *inst.ContainerID)
	}
	return c
}

// startAndWait starts an instance through the API and waits for the job to finish.
func startAndWait(t *testing.T, rt *Router, admin *store.User) {
	t.Helper()
	const instanceID = seededInstanceID
	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+instanceID+"/start", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "succeeded" {
		t.Fatalf("start job = %s, want succeeded (%s)", got.Status, deref(got.Error))
	}
}

// TestStartAppliesAnEditedLimit asserts that an edited resource limit reaches the container
// on the next start. A container's host config is fixed at creation, so this holds only if
// the start rebuilds it.
func TestStartAppliesAnEditedLimit(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	const wantMB = 2048
	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/inst-a",
		jsonBody(t, map[string]int{"mem_limit_mb": wantMB})))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	startAndWait(t, rt, admin)

	if got := currentContainer(t, db, fake).Spec.MemoryBytes; got != int64(wantMB)<<20 {
		t.Errorf("running container's memory limit = %d bytes, want %d", got, int64(wantMB)<<20)
	}
}

// TestStartAppliesAnEditedArgv asserts the same for the launch flags, which travel as Cmd
// against a fixed entrypoint (ADR-063) and so are equally unchangeable in place.
func TestStartAppliesAnEditedArgv(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	seed(t, db, `UPDATE instances SET crossplay = TRUE, server_name = 'Renamed' WHERE id = 'inst-a'`)
	startAndWait(t, rt, admin)

	argv := currentContainer(t, db, fake).Spec.Cmd
	if !slices.Contains(argv, "-crossplay") {
		t.Errorf("argv = %v, want it to carry -crossplay", argv)
	}
	if !containsArgValue(argv, "-name", "Renamed") {
		t.Errorf("argv = %v, want -name Renamed", argv)
	}
}

// TestRebuildPreservesSetOnceProperties asserts a rebuilt container keeps every set-once
// property of the one it replaces: the io.valmin.* labels reconciliation joins on (A2), the
// immutable -instanceid (A5), and A1/A3/A7's creation-time flags. Each fails silently.
func TestRebuildPreservesSetOnceProperties(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	oldID := seedInstance(t, rt, db, fake, "stopped")
	before := fake.Get(oldID)
	if before == nil {
		t.Fatal("seeded container is missing")
	}
	oldLabels := maps.Clone(before.Labels)

	seed(t, db, `UPDATE instances SET server_name = 'Renamed' WHERE id = 'inst-a'`)
	startAndWait(t, rt, admin)

	after := currentContainer(t, db, fake)
	if after.ID == oldID {
		t.Fatal("the container was not rebuilt, so this test proves nothing")
	}

	for _, label := range []string{
		instance.LabelManaged, instance.LabelSchema, instance.LabelInstanceID, instance.LabelBasePort,
	} {
		if after.Labels[label] != oldLabels[label] {
			t.Errorf("label %s = %q after rebuild, want %q (A2)",
				label, after.Labels[label], oldLabels[label])
		}
	}
	if !containsArgValue(after.Spec.Cmd, "-instanceid", "cp-inst-a") {
		t.Errorf("argv = %v, want -instanceid cp-inst-a preserved (A5)", after.Spec.Cmd)
	}

	// A1, A3, A7: properties that cannot be added to a container after creation.
	switch {
	case !after.Spec.OpenStdin:
		t.Error("OpenStdin is false on the rebuilt container (A1)")
	case after.Spec.StdinOnce:
		t.Error("StdinOnce is true on the rebuilt container (A1)")
	case after.Spec.TTY:
		t.Error("TTY is true on the rebuilt container (A1)")
	case after.Spec.User != before.Spec.User:
		t.Errorf("User = %q after rebuild, want %q (A3)", after.Spec.User, before.Spec.User)
	case after.Spec.RestartPolicy != "unless-stopped":
		t.Errorf("RestartPolicy = %q, want unless-stopped (A7)", after.Spec.RestartPolicy)
	case after.Spec.StopSignal != "SIGINT":
		t.Errorf("StopSignal = %q, want SIGINT (B1)", after.Spec.StopSignal)
	}
}

// TestStartWithoutDriftReusesTheContainer asserts an unchanged instance keeps its container.
// A needless rebuild re-runs every set-once decision, and would break ADR-107's premise that
// installing a mod needs no new container.
func TestStartWithoutDriftReusesTheContainer(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")

	startAndWait(t, rt, admin)
	settled := currentContainer(t, db, fake).ID

	stopAndWait(t, rt, admin)
	startAndWait(t, rt, admin)

	if got := currentContainer(t, db, fake).ID; got != settled {
		t.Errorf("container = %s after a second start, want %s reused — nothing had changed", got, settled)
	}
}

// TestStartRebuildsAContainerWithNoSpecHash asserts a container carrying no spec-hash label
// is rebuilt. Such a container predates the label and cannot be known to match its row.
func TestStartRebuildsAContainerWithNoSpecHash(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seeded := seedInstance(t, rt, db, fake, "stopped")

	// What an older panel left behind: 08 §1's four labels and nothing else.
	delete(fake.Get(seeded).Labels, instance.LabelSpecHash)

	startAndWait(t, rt, admin)

	if got := currentContainer(t, db, fake).ID; got == seeded {
		t.Error("a container carrying no spec hash was reused; it cannot be known to match the row")
	}
}

// TestRebuildThatCannotRecreateParksInError asserts that a failure between the remove and the
// create parks the instance in `error` rather than reporting a start that did not happen, and
// leaves worlds/ intact (B3).
func TestRebuildThatCannotRecreateParksInError(t *testing.T) {
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	worlds := instance.WorldsDir(rt.Supervisor().inst.Cfg.Data.HostRoot + "/instances/inst-a")
	if err := os.MkdirAll(worlds, 0o755); err != nil {
		t.Fatal(err)
	}

	seed(t, db, `UPDATE instances SET server_name = 'Renamed' WHERE id = 'inst-a'`)
	fake.CreateErr = errors.New("no such image")

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/inst-a/start", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "failed" {
		t.Fatalf("start job = %s, want failed — the container could not be recreated", got.Status)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = 'inst-a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
	if _, err := os.Stat(worlds); err != nil {
		t.Errorf("worlds/ did not survive a failed rebuild: %v", err)
	}
}

// stopAndWait stops an instance through the API and waits for the job to finish.
func stopAndWait(t *testing.T, rt *Router, admin *store.User) {
	t.Helper()
	const instanceID = seededInstanceID
	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+instanceID+"/stop", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "succeeded" {
		t.Fatalf("stop job = %s, want succeeded (%s)", got.Status, deref(got.Error))
	}
}

func containsArgValue(argv []string, flag, value string) bool {
	for i := range argv[:max(len(argv)-1, 0)] {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
