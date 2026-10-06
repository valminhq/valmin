package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// operationView is what an operator sees of an outstanding definition chain: the ordered
// steps and how far they got. The plan is not exposed — it is the chain's own input, not a
// progress report, and it carries the imported configuration bytes.
type operationView struct {
	ID        string                  `json:"id"`
	Kind      string                  `json:"kind"`
	State     string                  `json:"state"`
	Cursor    int                     `json:"cursor"`
	Steps     []control.OperationStep `json:"steps"`
	CreatedAt string                  `json:"created_at"`
	UpdatedAt string                  `json:"updated_at"`
}

func toOperationView(op *store.Operation, steps []control.OperationStep) operationView {
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	op, err := h.DB.OpenOperation(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if op == nil {
		// Settled is the ordinary answer, so it is a 200 carrying null rather than a 404:
		// ADR-038 reserves 404 here for an instance the caller cannot see, and a client
		// that got both could not tell them apart.
		JSON(w, r, http.StatusOK, nil)
		return
	}
	steps, _, err := control.DecodeOperation(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
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
		apierr.Write(w, r, apierr.New(errcode.InvalidState).With("operation_state", op.State))
		return
	}
	_, plan, err := control.DecodeOperation(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if op.State == store.OperationInterrupted {
		if err := h.DB.SetOperationState(r.Context(), op.ID, store.OperationRunning); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
	}
	job, err := h.ctl.Operations.SubmitStep(r.Context(), inst, steps[op.Cursor], &plan, u.ID)
	if err != nil {
		// Back to interrupted, or the chain would sit in `running` with nothing running it.
		if op.State == store.OperationInterrupted {
			_ = h.DB.SetOperationState(r.Context(), op.ID, store.OperationInterrupted)
		}
		writeJobSubmitError(w, r, err)
		return
	}
	// The step's job carries no audit entry of its own, so the resume is recorded once.
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.operation.resume",
		Detail: detailJSON(struct{}{}), IP: clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	op, steps, ok := h.mustLoadOperation(w, r, id)
	if !ok {
		return
	}
	if err := h.DB.SetOperationState(r.Context(), op.ID, store.OperationAbandoned); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.operation.abandon",
		Detail: detailJSON(struct{}{}), IP: clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	op.State = store.OperationAbandoned
	JSON(w, r, http.StatusOK, toOperationView(op, steps))
}

// mustLoadOperation reads the instance's outstanding operation with its steps decoded,
// answering 404 when the definition owes nothing.
func (h *Instances) mustLoadOperation(w http.ResponseWriter, r *http.Request, instanceID string) (
	*store.Operation, []control.OperationStep, bool,
) {
	op, err := h.DB.OpenOperation(r.Context(), instanceID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, nil, false
	}
	if op == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, nil, false
	}
	steps, _, err := control.DecodeOperation(op)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if op != nil {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
			With("operation_id", op.ID).With("operation_state", op.State))
		return false
	}
	return true
}
