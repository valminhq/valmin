package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Definition-operation kinds: what the chain was asked to build (ADR-116, ADR-151).
const (
	opKindCreate = "create"
	opKindImport = "import"
)

// opStep is one link of a definition chain. Kind is the job kind that performs it, Ref
// distinguishes steps of the same kind, and JobID is filled in when the step completes.
type opStep struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref,omitempty"`
	JobID string `json:"job_id,omitempty"`
}

// opPlan is the configuration the outstanding steps still need. It never carries the
// instance password or a browser-local file reference: both are re-supplied, not replayed.
type opPlan struct {
	Mods    []resolveRequest `json:"mods,omitempty"`
	Configs []manifestConfig `json:"configs,omitempty"`
	Start   bool             `json:"start_after_provision,omitempty"`
}

// planSteps lays out a definition chain: provision, one install per requested mod, the
// imported config write, then the requested start.
func planSteps(plan *opPlan) []opStep {
	steps := []opStep{{Kind: jobs.KindProvision.String()}}
	for _, m := range plan.Mods {
		steps = append(steps, opStep{Kind: jobs.KindModInstall.String(), Ref: m.FullName})
	}
	if len(plan.Configs) > 0 {
		steps = append(steps, opStep{Kind: jobs.KindConfigApply.String()})
	}
	if plan.Start {
		steps = append(steps, opStep{Kind: jobs.KindStart.String()})
	}
	return steps
}

// createOperation persists the chain's intent before the provision job is submitted, so a
// daemon that dies between two links still knows what the definition owes (Q52).
func (h *Instances) createOperation(
	ctx context.Context, instanceID, kind, createdBy string, plan *opPlan,
) error {
	steps, err := json.Marshal(planSteps(plan))
	if err != nil {
		return fmt.Errorf("encode operation steps: %w", err)
	}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode operation plan: %w", err)
	}
	var by *string
	if createdBy != "" {
		by = &createdBy
	}
	err = h.DB.CreateOperation(ctx, &store.Operation{
		ID: store.NewID(), InstanceID: instanceID, Kind: kind,
		State: store.OperationRunning, Steps: string(steps), Plan: string(encodedPlan),
		CreatedBy: by,
	})
	if err != nil {
		return fmt.Errorf("persist definition operation: %w", err)
	}
	return nil
}

// operationPlan decodes an operation's steps and plan together.
func operationPlan(op *store.Operation) (steps []opStep, plan opPlan, err error) {
	if err := json.Unmarshal([]byte(op.Steps), &steps); err != nil {
		return nil, plan, fmt.Errorf("decode operation %s steps: %w", op.ID, err)
	}
	if err := json.Unmarshal([]byte(op.Plan), &plan); err != nil {
		return nil, plan, fmt.Errorf("decode operation %s plan: %w", op.ID, err)
	}
	return steps, plan, nil
}

// AdvanceOperation is the job engine's finish hook. A succeeded job that matches the open
// operation's outstanding step records its id and moves the cursor, in the same transaction
// that made the job terminal; anything else leaves the operation untouched.
func (h *Instances) AdvanceOperation(ctx context.Context, tx *sql.Tx, fin jobs.FinishedJob) error {
	if fin.Status != jobs.StatusSucceeded || fin.InstanceID == nil {
		return nil
	}
	op, err := store.TxOpenOperation(ctx, tx, *fin.InstanceID)
	if err != nil {
		return fmt.Errorf("read open operation: %w", err)
	}
	if op == nil {
		return nil
	}
	steps, _, err := operationPlan(op)
	if err != nil {
		return err
	}
	if op.Cursor < 0 || op.Cursor >= len(steps) {
		return nil
	}
	step := steps[op.Cursor]
	if step.Kind != fin.Kind.String() || step.Ref != finishedRef(fin) {
		return nil
	}
	steps[op.Cursor].JobID = fin.ID
	encoded, err := json.Marshal(steps)
	if err != nil {
		return fmt.Errorf("encode operation %s steps: %w", op.ID, err)
	}
	cursor := op.Cursor + 1
	state := store.OperationRunning
	if cursor == len(steps) {
		state = store.OperationCompleted
	}
	if err := store.TxAdvanceOperation(ctx, tx, op.ID, string(encoded), cursor, state); err != nil {
		return fmt.Errorf("record completed step: %w", err)
	}
	return nil
}

// finishedRef is the step reference a finished job carries, empty for kinds that appear at
// most once in a chain.
func finishedRef(fin jobs.FinishedJob) string {
	if p, ok := fin.Payload.(modInstallPayload); ok {
		return p.FullName
	}
	return ""
}

// advanceChain submits the open operation's next outstanding step, with itself as that job's
// continuation. It does nothing for an instance with no operation, or one whose operation is
// interrupted, completed or abandoned: an interrupted chain waits for an explicit resume and
// is never replayed on the panel's own initiative.
func (h *Instances) advanceChain(ctx context.Context, instanceID string) {
	op, err := h.DB.OpenOperation(ctx, instanceID)
	if err != nil || op == nil || op.State != store.OperationRunning {
		if err != nil {
			slog.WarnContext(ctx, "read definition operation",
				slog.String("instance_id", instanceID), slog.Any("error", err))
		}
		return
	}
	steps, plan, err := operationPlan(op)
	if err != nil {
		slog.ErrorContext(ctx, "definition operation unreadable",
			slog.String("instance_id", instanceID), slog.Any("error", err))
		return
	}
	if op.Cursor >= len(steps) {
		return
	}
	inst, err := h.DB.InstanceByID(ctx, instanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "definition operation: instance vanished",
			slog.String("instance_id", instanceID), slog.Any("error", err))
		return
	}
	if _, err := h.submitStep(ctx, inst, steps[op.Cursor], &plan, deref(op.CreatedBy)); err != nil {
		// The chain stops here, leaving the operation outstanding for an explicit resume.
		slog.WarnContext(ctx, "definition operation step not submitted",
			slog.String("instance_id", instanceID),
			slog.String("step", steps[op.Cursor].Kind), slog.Any("error", err))
	}
}

// submitStep dispatches one chain step, chaining advanceChain behind the steps that have a
// successor, and reports the job that now holds the instance lock.
func (h *Instances) submitStep(
	ctx context.Context, inst *store.Instance, step opStep, plan *opPlan, requestedBy string,
) (*store.Job, error) {
	next := func(ctx context.Context) { h.advanceChain(ctx, inst.ID) }
	switch step.Kind {
	case jobs.KindModInstall.String():
		if h.Mods == nil {
			// The operator asked for a modded server: starting it vanilla would generate the
			// world under a definition that promises mods.
			return nil, fmt.Errorf("mods requested but no mod engine is wired")
		}
		req, ok := planMod(plan, step.Ref)
		if !ok {
			return nil, fmt.Errorf("operation plan has no mod %s", step.Ref)
		}
		job, err := h.Mods.SubmitInstall(ctx, inst, req, requestedBy, next)
		if err != nil {
			return nil, fmt.Errorf("submit install of %s: %w", step.Ref, err)
		}
		return job, nil
	case jobs.KindConfigApply.String():
		return h.submitConfigApply(ctx, inst, plan.Configs, requestedBy, next)
	case jobs.KindStart.String():
		if inst.ContainerID == nil {
			return nil, fmt.Errorf("instance %s has no container to start", inst.ID)
		}
		return h.submitStart(ctx, inst, *inst.ContainerID, requestedBy)
	default:
		return nil, fmt.Errorf("no chain step defined for kind %s", step.Kind)
	}
}

// planMod finds the requested version of a mod named by a step.
func planMod(plan *opPlan, fullName string) (resolveRequest, bool) {
	for _, m := range plan.Mods {
		if m.FullName == fullName {
			return m, true
		}
	}
	return resolveRequest{}, false
}

// operationView is what an operator sees of an outstanding definition chain: the ordered
// steps and how far they got. The plan is not exposed — it is the chain's own input, not a
// progress report, and it carries the imported configuration bytes.
type operationView struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	State     string   `json:"state"`
	Cursor    int      `json:"cursor"`
	Steps     []opStep `json:"steps"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func toOperationView(op *store.Operation, steps []opStep) operationView {
	return operationView{
		ID: op.ID, Kind: op.Kind, State: op.State, Cursor: op.Cursor, Steps: steps,
		CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt,
	}
}

// operation is GET /instances/{id}/operation: the outstanding definition chain, or null once
// there is none. Visibility is the only requirement to read it (ADR-038).
func (h *Instances) operation(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	op, err := h.DB.OpenOperation(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if op == nil {
		// Settled is the ordinary answer, so it is a 200 carrying null rather than a 404:
		// ADR-038 reserves 404 here for an instance the caller cannot see, and a client
		// that got both could not tell them apart.
		JSON(w, r, http.StatusOK, nil)
		return
	}
	steps, _, err := operationPlan(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toOperationView(op, steps))
}

// resumeOperation is POST /instances/{id}/operation/resume: submit the outstanding step of a
// chain a crash cut. Resuming is the creation authority, not the instance's own operator, so
// it is gated the way creating the definition was (09 §3.3).
//
// A second resume while the first is still running is not a second claim: the step it would
// submit collides with the instance lock the first one holds, and answers 409 job_in_progress
// naming it (11 §3).
func (h *Instances) resumeOperation(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	op, steps, ok := h.mustLoadOperation(w, r, id)
	if !ok {
		return
	}
	if op.Cursor >= len(steps) {
		apierr.Write(w, r, apierr.New(apierr.InvalidState).With("operation_state", op.State))
		return
	}
	_, plan, err := operationPlan(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if op.State == store.OperationInterrupted {
		if err := h.DB.SetOperationState(r.Context(), op.ID, store.OperationRunning); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
	}
	job, err := h.submitStep(r.Context(), inst, steps[op.Cursor], &plan, u.ID)
	if err != nil {
		// Back to interrupted, or the chain would sit in `running` with nothing running it.
		if op.State == store.OperationInterrupted {
			_ = h.DB.SetOperationState(r.Context(), op.ID, store.OperationInterrupted)
		}
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// abandonOperation is POST /instances/{id}/operation/abandon: give up the outstanding intent
// and leave the instance as the landed steps built it. Nothing already installed or written is
// undone; only the steps that never ran are dropped, and a job still in flight finishes and
// then finds no open operation to advance.
func (h *Instances) abandonOperation(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	op, steps, ok := h.mustLoadOperation(w, r, id)
	if !ok {
		return
	}
	if err := h.DB.SetOperationState(r.Context(), op.ID, store.OperationAbandoned); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	op.State = store.OperationAbandoned
	JSON(w, r, http.StatusOK, toOperationView(op, steps))
}

// mustLoadOperation reads the instance's outstanding operation with its steps decoded,
// answering 404 when the definition owes nothing.
func (h *Instances) mustLoadOperation(w http.ResponseWriter, r *http.Request, instanceID string) (
	*store.Operation, []opStep, bool,
) {
	op, err := h.DB.OpenOperation(r.Context(), instanceID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, nil, false
	}
	if op == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, nil, false
	}
	steps, _, err := operationPlan(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, nil, false
	}
	return op, steps, true
}

// operationSettled reports whether the instance owes no outstanding definition step, and is the
// guard on every endpoint that starts the server or changes what it is defined as (ADR-164). A
// chain a crash cut leaves the instance `stopped` with its lock free, so nothing else holds
// either back, and the outstanding steps would later overwrite the change (Q52).
//
// Writes the response and reports false when an operation is still open.
func operationSettled(w http.ResponseWriter, r *http.Request, db *store.DB, instanceID string) bool {
	op, err := db.OpenOperation(r.Context(), instanceID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if op != nil {
		apierr.Write(w, r, apierr.New(apierr.InvalidState).
			With("operation_id", op.ID).With("operation_state", op.State))
		return false
	}
	return true
}
