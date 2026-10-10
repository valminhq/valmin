package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// TestACutChainIsMarkedInterrupted asserts that a definition operation left running by a dead
// process is reported as interrupted at startup, with its landed steps intact.
func TestACutChainIsMarkedInterrupted(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-cut")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{
		Mods: []manager.PackageRequest{{FullName: "A-One", Version: "1.0.0"}},
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
	h := rt.instances
	engine := &fakeModEngine{t: t, h: h, db: db}
	useModEngine(h, engine)

	inst := seedStoppedInstance(t, db, "chain-interrupted")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{
		Mods: []manager.PackageRequest{{FullName: "A-One", Version: "1.0.0"}},
	})
	op, err := db.OpenOperation(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetOperationState(t.Context(), op.ID, store.OperationInterrupted); err != nil {
		t.Fatal(err)
	}

	h.ctl.Operations.Advance(t.Context(), inst.ID)

	if len(engine.installed) != 0 {
		t.Errorf("installed %v, want nothing replayed without an explicit resume", engine.installed)
	}
}

// TestACompletedStepIsNotRepeated asserts that the chain picks up at its cursor: a mod whose
// install job already finished is not installed a second time.
func TestACompletedStepIsNotRepeated(t *testing.T) {
	rt, db, _, _ := provisionWorld(t)
	h := rt.instances
	engine := &fakeModEngine{t: t, h: h, db: db}
	useModEngine(h, engine)

	inst := seedStoppedInstance(t, db, "chain-resumed")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
		{FullName: "A-One", Version: "1.0.0"},
		{FullName: "B-Two", Version: "2.0.0"},
	}})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindModInstall, manager.InstallPayload{FullName: "A-One"})

	h.ctl.Operations.Advance(t.Context(), inst.ID)

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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-unrelated")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
		{FullName: "A-One", Version: "1.0.0"},
	}})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindBackup, struct{}{})
	finishStep(t, h, db, t.Context(), inst.ID, jobs.KindModInstall, manager.InstallPayload{FullName: "Other-Mod"})

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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-progress")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{
		Mods:    []manager.PackageRequest{{FullName: "A-One", Version: "1.0.0"}},
		Configs: []control.ManifestConfig{{File: "BepInEx/config/x.cfg", Content: "[General]\nsecretline = 1"}},
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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-authority")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
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
	h := rt.instances
	engine := &fakeModEngine{t: t, h: h, db: db}
	useModEngine(h, engine)

	inst := seedStoppedInstance(t, db, "chain-resume")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-locked")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{
		Configs: []control.ManifestConfig{{File: "BepInEx/config/x.cfg", Content: "[General]\n"}},
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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-failed-step")
	if err := h.ctl.Operations.
		Create(t.Context(), inst.ID, control.OperationCreate, "", &control.OperationPlan{Start: true}); err != nil {
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

// TestResumeRetriesAFailedInstall asserts that resuming a chain whose install failed runs the
// install again, and that a retry which fails too leaves the server parked and still resumable.
func TestResumeRetriesAFailedInstall(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	inst := seedFailedInstall(t, rt, db, "install-retry")
	rt.instances.Runtime.(*runtime.Fake).ExitCodes = []int{1}

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/resume", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	if stub.Kind != jobs.KindProvision.String() {
		t.Errorf("kind = %q, want the install retried", stub.Kind)
	}
	if got := waitJob(t, rt, admin, stub.JobID); got.Status != "failed" {
		t.Fatalf("retried install = %s, want failed against the scripted SteamCMD exit", got.Status)
	}

	row, err := db.InstanceByID(t.Context(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "error" {
		t.Errorf("state = %s, want error after the retry failed", row.State)
	}
	assertInstallOutstanding(t, db, inst.ID)
}

// TestResumeOfAnInstallNeedsTheParkedState asserts that the install is retried only from the
// state a failed install leaves, and that any other state is a 409 that spends nothing.
func TestResumeOfAnInstallNeedsTheParkedState(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	inst := seedFailedInstall(t, rt, db, "install-not-parked")
	seed(t, db, `UPDATE instances SET state = 'stopped' WHERE id = ?`, inst.ID)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/resume", http.NoBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := errCode(t, rec); got != "invalid_state" {
		t.Errorf("error code = %q, want invalid_state", got)
	}
	assertInstallOutstanding(t, db, inst.ID)
}

// TestAFailedInstallCannotBeSkippedOrCleared asserts that abandoning the chain and acknowledging
// the parked state both answer 409 and change nothing while the install is outstanding.
func TestAFailedInstallCannotBeSkippedOrCleared(t *testing.T) {
	for _, path := range []string{"operation/abandon", "acknowledge"} {
		t.Run(path, func(t *testing.T) {
			rt, db, admin, _ := provisionWorld(t)
			inst := seedFailedInstall(t, rt, db, "install-held")

			rec := as(rt, admin, httptest.NewRequest(
				http.MethodPost, "/api/v1/instances/"+inst.ID+"/"+path, http.NoBody))
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body)
			}
			if got := errCode(t, rec); got != "invalid_state" {
				t.Errorf("error code = %q, want invalid_state", got)
			}
			row, err := db.InstanceByID(t.Context(), inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != "error" {
				t.Errorf("state = %s, want the server still parked", row.State)
			}
			assertInstallOutstanding(t, db, inst.ID)
		})
	}
}

// seedFailedInstall leaves an instance the way a failed first install does: parked in error
// with no container, a stored password, and its chain interrupted on the install step.
func seedFailedInstall(t *testing.T, rt *Server, db *store.DB, name string) *store.Instance {
	t.Helper()
	inst := seedStoppedInstance(t, db, name)
	envelope, err := rt.instances.Keeper.Encrypt(
		crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(inst.ID), []byte("hunter22"))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instances SET state = 'error', password = ? WHERE id = ?`, envelope, inst.ID)
	if err := rt.instances.ctl.Operations.Create(
		t.Context(), inst.ID, control.OperationCreate, "", &control.OperationPlan{Start: true}); err != nil {
		t.Fatal(err)
	}
	failStep(t, rt.instances, db, inst.ID, jobs.KindProvision)
	return inst
}

// assertInstallOutstanding fails unless the instance's chain is interrupted on its install step.
func assertInstallOutstanding(t *testing.T, db *store.DB, instanceID string) {
	t.Helper()
	op, err := db.OpenOperation(t.Context(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || op.State != store.OperationInterrupted || op.Cursor != 0 {
		t.Errorf("operation = %+v, want it interrupted with the install outstanding", op)
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
	if err := h.ctl.Operations.OnJobFinished(t.Context(), tx, &jobs.FinishedJob{
		ID: store.NewID(), Kind: kind, InstanceID: &instanceID,
		Payload: control.ProvisionPayload{}, Status: jobs.StatusFailed,
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
	h := rt.instances
	engine := &fakeModEngine{t: t, h: h, db: db}
	useModEngine(h, engine)

	inst := seedStoppedInstance(t, db, "chain-abandon")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
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

	h.ctl.Operations.Advance(t.Context(), inst.ID)
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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-incomplete")
	setContainerID(t, db, inst.ID, "container-incomplete")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
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
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-frozen")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Mods: []manager.PackageRequest{
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
func holdInstanceLock(t *testing.T, rt *Server, u *store.User, inst *store.Instance) string {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	id := inst.ID
	holder, err := rt.instances.Engine.Submit(t.Context(), &jobs.Spec{
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

// TestResumeWritesOneAuditEntryAndNoneForItsStep asserts that resuming a chain is recorded once,
// by the resume itself, and that the step it submits adds no second row.
func TestResumeWritesOneAuditEntryAndNoneForItsStep(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-audit")
	setContainerID(t, db, inst.ID, "container-1")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Start: true})
	interrupt(t, db, inst.ID)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/resume", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var stub jobView
	decodeInto(t, rec, &stub)
	waitJob(t, rt, admin, stub.JobID)

	want := lifecycleAuditRow{
		UserID: admin.ID, ActorName: admin.Username, InstanceID: inst.ID, InstanceName: inst.Name,
		Action: "instances.operation.resume", Detail: `{}`, IP: "192.0.2.1", Outcome: store.AuditSucceeded,
	}
	if got := lifecycleAuditRows(t, db, want.Action); len(got) != 1 || got[0] != want {
		t.Errorf("resume audit rows = %+v, want exactly %+v", got, want)
	}
	if got := lifecycleAuditRows(t, db, "instances.start"); len(got) != 0 {
		t.Errorf("start audit rows = %+v, want none: the resume already records the chain step", got)
	}
}

// TestAbandonWritesAnAuditEntry asserts that giving up a chain is recorded against the instance.
func TestAbandonWritesAnAuditEntry(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	h := rt.instances

	inst := seedStoppedInstance(t, db, "chain-abandon-audit")
	seedChain(t, h, db, inst.ID, &control.OperationPlan{Start: true})

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, "/api/v1/instances/"+inst.ID+"/operation/abandon", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	want := lifecycleAuditRow{
		UserID: admin.ID, ActorName: admin.Username, InstanceID: inst.ID, InstanceName: inst.Name,
		Action: "instances.operation.abandon", Detail: `{}`, IP: "192.0.2.1", Outcome: store.AuditSucceeded,
	}
	if got := lifecycleAuditRows(t, db, want.Action); len(got) != 1 || got[0] != want {
		t.Errorf("abandon audit rows = %+v, want exactly %+v", got, want)
	}
}
