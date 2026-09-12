package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// TestACutChainIsMarkedInterrupted asserts that a definition operation left running by a dead
// process is reported as interrupted at startup, with its landed steps intact.
func TestACutChainIsMarkedInterrupted(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-cut")
	seedChain(t, h, db, inst.ID, &opPlan{
		Mods: []resolveRequest{{FullName: "A-One", Version: "1.0.0"}},
	})

	if err := rt.supervisor.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}

	op, err := db.OpenOperation(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op == nil {
		t.Fatal("the cut chain was closed out instead of reported")
	}
	if op.State != store.OperationInterrupted {
		t.Errorf("state = %s, want %s", op.State, store.OperationInterrupted)
	}
	if op.Cursor != 1 {
		t.Errorf("cursor = %d, want the completed provision step preserved", op.Cursor)
	}
}

// TestAnInterruptedChainIsNotReplayed asserts that nothing continues a cut chain on the
// panel's own initiative: the remaining work waits for an explicit resume.
func TestAnInterruptedChainIsNotReplayed(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst
	engine := &fakeModEngine{t: t, h: h, db: db}
	h.Mods = engine

	inst := seedStoppedInstance(t, db, "chain-interrupted")
	seedChain(t, h, db, inst.ID, &opPlan{
		Mods: []resolveRequest{{FullName: "A-One", Version: "1.0.0"}},
	})
	op, err := db.OpenOperation(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetOperationState(t.Context(), op.ID, store.OperationInterrupted); err != nil {
		t.Fatal(err)
	}

	h.advanceChain(t.Context(), inst.ID)

	if len(engine.installed) != 0 {
		t.Errorf("installed %v, want nothing replayed without an explicit resume", engine.installed)
	}
}

// TestACompletedStepIsNotRepeated asserts that the chain picks up at its cursor: a mod whose
// install job already finished is not installed a second time.
func TestACompletedStepIsNotRepeated(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst
	engine := &fakeModEngine{t: t, h: h, db: db}
	h.Mods = engine

	inst := seedStoppedInstance(t, db, "chain-resumed")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
		{FullName: "B-Two", Version: "2.0.0"},
	}})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindModInstall, modInstallPayload{FullName: "A-One"})

	h.advanceChain(t.Context(), inst.ID)

	if len(engine.installed) != 1 || engine.installed[0] != "B-Two" {
		t.Errorf("installed %v, want only the outstanding package", engine.installed)
	}
	if op, err := db.OpenOperation(t.Context(), inst.ID); err != nil {
		t.Fatal(err)
	} else if op != nil {
		t.Errorf("operation still open at step %d after every step landed", op.Cursor)
	}
}

// TestAnUnrelatedJobDoesNotAdvanceTheChain asserts that the finish hook advances only on the
// step it is waiting for, so a job an operator ran alongside cannot report the chain complete.
func TestAnUnrelatedJobDoesNotAdvanceTheChain(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-unrelated")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindBackup, struct{}{})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindModInstall, modInstallPayload{FullName: "Other-Mod"})

	op, err := db.OpenOperation(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || op.Cursor != 1 {
		t.Fatalf("operation = %+v, want the install step still outstanding", op)
	}
}

// TestOperationProgressIsScopedToTheInstance asserts that reading a chain's progress needs
// visibility of the instance it belongs to, and that a caller without it cannot tell the
// operation apart from one that does not exist (ADR-038).
func TestOperationProgressIsScopedToTheInstance(t *testing.T) {
	rt, db, admin, member := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-progress")
	seedChain(t, h, db, inst.ID, &opPlan{
		Mods:    []resolveRequest{{FullName: "A-One", Version: "1.0.0"}},
		Configs: []manifestConfig{{File: "BepInEx/config/x.cfg", Content: "[General]\nsecretline = 1"}},
	})
	path := "/api/v1/instances/" + inst.ID + "/operation"

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("invisible instance = %d, want 404 (%s)", rec.Code, rec.Body)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got operationView
	decodeInto(t, rec, &got)
	if got.Cursor != 1 || len(got.Steps) != 3 {
		t.Errorf("progress = cursor %d of %d steps, want the install outstanding", got.Cursor, len(got.Steps))
	}
	if got.Steps[0].JobID == "" {
		t.Error("the landed provision step reports no job id")
	}
	if strings.Contains(rec.Body.String(), "secretline") {
		t.Errorf("progress exposes the chain's plan: %s", rec.Body)
	}
}

// TestASettledInstanceReportsNoOperation asserts that owing nothing is a 200 carrying null,
// not a 404: ADR-038 gives 404 one meaning here, and a client that got it for both could not
// tell an instance it cannot see from one whose chain finished.
func TestASettledInstanceReportsNoOperation(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)

	inst := seedStoppedInstance(t, db, "chain-settled")
	rec := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/"+inst.ID+"/operation", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "null" {
		t.Errorf("body = %s, want null", body)
	}
}

// TestResumingIsTheCreationAuthority asserts that seeing an instance is not enough to drive
// its definition forward: resuming builds the instance, so it is gated like creating it.
func TestResumingIsTheCreationAuthority(t *testing.T) {
	rt, db, _, member := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-authority")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES (?, ?, 'operator', '[]', ?)`, member.ID, inst.ID, store.Now())

	for _, action := range []string{"resume", "abandon"} {
		rec := as(rt, member, httptest.NewRequest(
			http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/"+action, http.NoBody))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s as an operator = %d, want 403 (%s)", action, rec.Code, rec.Body)
		}
	}
	if op, err := db.OpenOperation(t.Context(), inst.ID); err != nil {
		t.Fatal(err)
	} else if op.State != store.OperationRunning || op.Cursor != 1 {
		t.Errorf("operation = %s at step %d, want it untouched", op.State, op.Cursor)
	}
}

// TestResumeRunsTheOutstandingStepOnce asserts that an explicit resume submits the step the
// chain still owes, and that a second resume has nothing left to claim.
func TestResumeRunsTheOutstandingStepOnce(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.supervisor.inst
	engine := &fakeModEngine{t: t, h: h, db: db}
	h.Mods = engine

	inst := seedStoppedInstance(t, db, "chain-resume")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	interrupt(t, db, inst.ID)
	path := "/api/v1/instances/" + inst.ID + "/operation/resume"

	rec := as(rt, admin, httptest.NewRequest(http.MethodPost, path, http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodPost, path, http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("second resume = %d, want 404 with nothing left owed (%s)", rec.Code, rec.Body)
	}
	if len(engine.installed) != 1 {
		t.Errorf("installed %v, want the outstanding package exactly once", engine.installed)
	}
}

// TestAResumeThatCannotClaimLeavesTheChainResumable asserts that a resume colliding with a job
// already on the instance answers 409 naming it and spends nothing: the intent is still
// interrupted afterwards, so the operator can try again once the lock is free.
func TestAResumeThatCannotClaimLeavesTheChainResumable(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-locked")
	seedChain(t, h, db, inst.ID, &opPlan{
		Configs: []manifestConfig{{File: "BepInEx/config/x.cfg", Content: "[General]\n"}},
	})
	interrupt(t, db, inst.ID)
	holder := holdInstanceLock(t, rt, admin, inst)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/resume", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "job_in_progress" {
		t.Errorf("error code = %q, want job_in_progress", got)
	}
	if !strings.Contains(rec.Body.String(), holder) {
		t.Errorf("conflict does not identify the holder %q: %s", holder, rec.Body)
	}
	if op, err := db.OpenOperation(t.Context(), inst.ID); err != nil {
		t.Fatal(err)
	} else if op.State != store.OperationInterrupted {
		t.Errorf("state = %s, want the chain still resumable", op.State)
	}
}

// TestAFailedStepInterruptsTheChain asserts that a step that ends without landing leaves the
// operation resumable rather than in `running` with nothing running it. Only the startup pass
// used to make that correction, so within one daemon lifetime the chain never left `running`
// and the instance stayed blocked with no resume affordance pointing at it.
func TestAFailedStepInterruptsTheChain(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-failed-step")
	if err := h.createOperation(t.Context(), inst.ID, opKindCreate, "", &opPlan{Start: true}); err != nil {
		t.Fatal(err)
	}

	failStep(t, h, db, inst.ID, jobs.KindProvision)

	op, err := db.OpenOperation(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op == nil {
		t.Fatal("the failed chain left no operation to resume")
	}
	if op.State != store.OperationInterrupted {
		t.Errorf("state = %s, want interrupted", op.State)
	}
	if op.Cursor != 0 {
		t.Errorf("cursor = %d, want the failed step still outstanding", op.Cursor)
	}
}

// failStep runs the finish hook for a job of this kind that did not succeed.
func failStep(t *testing.T, h *Instances, db *store.DB, instanceID string, kind jobs.Kind) {
	t.Helper()
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := h.AdvanceOperation(t.Context(), tx, &jobs.FinishedJob{
		ID: store.NewID(), Kind: kind, InstanceID: &instanceID,
		Payload: provisionPayload{}, Status: jobs.StatusFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestAbandonKeepsWhatLandedAndStopsTheChain asserts that abandoning drops only the steps that
// never ran: the completed ones keep their recorded job, and nothing is submitted afterwards.
func TestAbandonKeepsWhatLandedAndStopsTheChain(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.supervisor.inst
	engine := &fakeModEngine{t: t, h: h, db: db}
	h.Mods = engine

	inst := seedStoppedInstance(t, db, "chain-abandon")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/abandon", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var got operationView
	decodeInto(t, rec, &got)
	if got.State != store.OperationAbandoned || got.Steps[0].JobID == "" {
		t.Errorf("abandoned operation = %+v, want the landed provision step preserved", got)
	}

	h.advanceChain(t.Context(), inst.ID)
	if len(engine.installed) != 0 {
		t.Errorf("installed %v after abandoning, want nothing", engine.installed)
	}
	if op, err := db.OpenOperation(t.Context(), inst.ID); err != nil {
		t.Fatal(err)
	} else if op != nil {
		t.Errorf("operation still open in %s after abandoning", op.State)
	}
}

// TestAnIncompleteInstanceCannotBeStarted asserts that a chain cut before its mods landed
// blocks start: the instance is stopped with a free lock, so nothing else would, and the first
// boot is what writes the world.
func TestAnIncompleteInstanceCannotBeStarted(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-incomplete")
	setContainerID(t, db, inst.ID, "container-incomplete")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	interrupt(t, db, inst.ID)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/start", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", got)
	}

	rec = as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/abandon", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("abandon = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rec = as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/start", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Errorf("start after abandoning = %d, want 202 (%s)", rec.Code, rec.Body)
	}
}

// TestAnIncompleteDefinitionCannotBeChanged asserts that the settings, mod and configuration
// endpoints are held back alongside start: the outstanding steps carry the definition the
// instance was asked for, and would later overwrite whatever was changed under them.
func TestAnIncompleteDefinitionCannotBeChanged(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.supervisor.inst

	inst := seedStoppedInstance(t, db, "chain-frozen")
	seedChain(t, h, db, inst.ID, &opPlan{Mods: []resolveRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	interrupt(t, db, inst.ID)

	rec := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/"+inst.ID,
		jsonBody(t, map[string]any{"server_name": "Renamed"})))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Fatalf("patch = %d %s, want 409 invalid_state (%s)", rec.Code, errCode(t, rec), rec.Body)
	}

	rec = as(rt, admin, httptest.NewRequest(http.MethodPost, "/api/v1/instances/"+inst.ID+"/mods",
		jsonBody(t, map[string]any{"mods": []map[string]string{{"full_name": "B-Two", "version": "1.0.0"}}})))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "invalid_state" {
		t.Errorf("mod install = %d %s, want 409 invalid_state (%s)", rec.Code, errCode(t, rec), rec.Body)
	}
}

// interrupt puts a seeded chain in the state a crash leaves it in.
func interrupt(t *testing.T, db *store.DB, instanceID string) {
	t.Helper()
	op, err := db.OpenOperation(t.Context(), instanceID)
	if err != nil || op == nil {
		t.Fatalf("read seeded operation: %v", err)
	}
	if err := db.SetOperationState(t.Context(), op.ID, store.OperationInterrupted); err != nil {
		t.Fatal(err)
	}
}

// holdInstanceLock parks a job on the instance for the rest of the test and reports its id.
func holdInstanceLock(t *testing.T, rt *Router, u *store.User, inst *store.Instance) string {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	id := inst.ID
	holder, err := rt.supervisor.inst.Engine.Submit(t.Context(), &jobs.Spec{
		Kind: jobs.KindBackup, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, Payload: struct{}{},
	}, func(context.Context, *jobs.Handle) jobs.Outcome {
		close(started)
		<-release
		return jobs.Outcome{Status: "succeeded"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	t.Cleanup(func() {
		close(release)
		waitJob(t, rt, u, holder.ID)
	})
	return holder.ID
}
