package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Instances serves 04 §3's instance surface: creation (a job, WP-M1-13), the read-side
// CRUD, the limited PATCH M1 defines, the audited password endpoint, the one way out of
// `error`, and the lifecycle jobs — start, stop, restart, delete (WP-M1-14, lifecycle.go).
type Instances struct {
	DB      *store.DB
	Authz   *authz.Authz
	Runtime runtime.Runtime
	Keeper  *crypto.Keeper
	Engine  *jobs.Engine
	Cfg     *config.Config
}

func (h *Instances) Routes(rt *Router) {
	rt.Handle("GET /api/v1/instances", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/instances", http.HandlerFunc(h.create))
	// Ahead of /instances/{id}: ServeMux prefers the literal segment, so "orphans" cannot
	// be read as an id, but registering it first keeps that obvious to a reader too.
	rt.Handle("GET /api/v1/instances/orphans", http.HandlerFunc(h.orphans))
	rt.Handle("GET /api/v1/instances/{id}", http.HandlerFunc(h.get))
	rt.Handle("PATCH /api/v1/instances/{id}", http.HandlerFunc(h.patch))
	rt.Handle("GET /api/v1/instances/{id}/password", http.HandlerFunc(h.password))
	rt.Handle("POST /api/v1/instances/{id}/acknowledge", http.HandlerFunc(h.acknowledge))
	rt.Handle("POST /api/v1/instances/{id}/start", http.HandlerFunc(h.start))
	rt.Handle("POST /api/v1/instances/{id}/stop", http.HandlerFunc(h.stop))
	rt.Handle("POST /api/v1/instances/{id}/restart", http.HandlerFunc(h.restart))
	rt.Handle("DELETE /api/v1/instances/{id}", http.HandlerFunc(h.delete))
	h.listRoutes(rt)
}

// list is GET /instances: every instance for admin, grant-scoped for a member (09 §1).
func (h *Instances) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	ids, all, err := h.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if all {
		ids = nil // ListInstances(nil) is "every row"; VisibleInstances(all=true) carries none
	}
	instances, err := h.DB.ListInstances(r.Context(), ids)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(instances, nil))
}

// get is GET /instances/{id}. An instance the caller cannot see does not exist (D2,
// ADR-038) — the same 404 for "no such id" and "not yours to see".
func (h *Instances) get(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	JSON(w, r, http.StatusOK, inst)
}

type patchInstanceRequest struct {
	MemLimitMB *int     `json:"mem_limit_mb"`
	CPULimit   *float64 `json:"cpu_limit"`
	ExtraArgs  *string  `json:"extra_args"`
}

// mergeInstanceLimits is PATCH semantics (11 §1.1): absent means unchanged, so every field
// starts from current and only what body actually set overrides it.
func mergeInstanceLimits(current *store.Instance, body patchInstanceRequest) store.InstanceLimits {
	patch := store.InstanceLimits{
		MemLimitMB: current.MemLimitMB,
		CPULimit:   current.CPULimit,
		ExtraArgs:  current.ExtraArgs,
	}
	if body.MemLimitMB != nil {
		patch.MemLimitMB = *body.MemLimitMB
	}
	if body.CPULimit != nil {
		patch.CPULimit = body.CPULimit
	}
	if body.ExtraArgs != nil {
		patch.ExtraArgs = body.ExtraArgs
	}
	return patch
}

// patch is PATCH /instances/{id}. `↯` M1 scope, recorded rather than silently chosen: it
// carries only mem_limit_mb, cpu_limit and extra_args — the two 09 §3.3 already names
// (InstanceLimits, InstanceExtraArgs) — because 09 §3 defines no action for editing the
// rest of "launch config" (server_name, world_name, password, preset, modifiers, public,
// crossplay). world_name in particular is a file-rename operation (03 §4.1), not a bare
// column write, and does not belong behind a plain PATCH regardless. Gap flagged in the
// M1 plan's write-back rather than an action invented here.
func (h *Instances) patch(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	var body patchInstanceRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if (body.MemLimitMB != nil || body.CPULimit != nil) && !h.Authz.Can(r.Context(), u, authz.InstanceLimits, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	if body.ExtraArgs != nil && !h.Authz.Can(r.Context(), u, authz.InstanceExtraArgs, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}

	current, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if current == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	if err := h.DB.UpdateInstanceLimits(r.Context(), id, mergeInstanceLimits(current, body)); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}

type instancePassword struct {
	Password string `json:"password"`
}

// password is GET /instances/{id}/password (11 §9): a live game secret, not a credential
// to verify, readable by any caller with instance.view but kept out of every other
// payload — and every read of this one writes an audit_log row.
func (h *Instances) password(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	exists, err := h.DB.InstanceExists(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if !exists {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	envelope, err := h.DB.InstancePassword(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	plaintext, err := h.Keeper.Decrypt(
		crypto.PurposeInstancePassword, crypto.Location{Table: "instances", Column: "password", RowID: id}, envelope,
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.password.read",
		IP: middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, instancePassword{Password: string(plaintext)})
}

// acknowledge is POST /instances/{id}/acknowledge (12 §2.4): the only way out of `error`.
// It re-runs the observer's own reconciliation question for this one instance and lands
// where reality supports — deliberately not "clear the flag" — rather than trusting that
// whatever caused the error has been fixed.
func (h *Instances) acknowledge(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if instance.State(inst.State) != instance.StateError {
		apierr.Write(w, r, apierr.New(apierr.InvalidState).
			With("state", inst.State).
			With("allowed_states", []instance.State{instance.StateError}))
		return
	}

	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}
	next, err := instance.Reconcile(r.Context(), h.Runtime, containerID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	if _, err := h.DB.UpdateInstanceState(r.Context(), id, inst.State, string(next)); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}
