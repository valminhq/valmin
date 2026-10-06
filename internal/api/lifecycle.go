package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// checkInstanceState is 12 §3.1's "Requires" column, checked before a job is submitted, so a
// client gets 409 invalid_state with allowed_states rather than racing the engine's
// compare-and-swap inside OnClaim.
func checkInstanceState(w http.ResponseWriter, r *http.Request, inst *store.Instance, kind jobs.Kind) bool {
	allowed := instance.AllowedFrom(kind)
	for _, s := range allowed {
		if instance.State(inst.State) == s {
			return true
		}
	}
	apierr.Write(w, r, apierr.New(errcode.InvalidState).With("state", inst.State).With("allowed_states", allowed))
	return false
}

// writeJobSubmitError is the ADR-030 shape every job-creating endpoint answers with: a lock
// collision is 409 job_in_progress naming the active job, never a queued placeholder.
func writeJobSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrBackupProtected) {
		apierr.Write(
			w,
			r,
			apierr.New(errcode.InvalidState).Msg("Cancel pending remote uploads before deleting this server."),
		)
		return
	}
	if errors.Is(err, jobs.ErrShuttingDown) {
		apierr.Write(w, r, apierr.New(errcode.Unavailable))
		return
	}
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		apierr.Write(w, r, apierr.New(errcode.JobInProgress).With("job_id", conflict.JobID))
		return
	}
	apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
}

// start is POST /instances/{id}/start (04 §3, ADR-028): `stopped` only — `start` from
// `error` is not permitted (12 §2.4), because a parked instance has probable world damage.
func (h *Instances) start(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceStart, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindStart) {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}
	containerID, ok := h.mustHaveContainer(w, r, inst)
	if !ok {
		return
	}

	audit := jobAudit(r.Context(), u.ID, id, "instances.start", struct{}{})
	job, err := h.submitStart(r.Context(), inst, containerID, u.ID, audit)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitStart claims `stopped → starting` and dispatches the start job. Its three callers are
// POST /instances/{id}/start, start_after_provision (ADR-033) and a resume intent (12 §9.3), so
// all of them enter `starting` through one claim. audit is the trail entry the claim writes; the
// endpoint supplies it and the other two pass nil, since they continue work already on record.
func (h *Instances) submitStart(
	ctx context.Context, inst *store.Instance, containerID, requestedBy string, audit *store.AuditEntry,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.starter().Submit(ctx, &control.StartSubmission{
		Instance: inst, ContainerID: containerID, RequestedBy: requestedBy, Audit: audit,
	})
}

// pluginLoadWindow bounds the optional plugin-count evidence after readiness.
var pluginLoadWindow = 5 * time.Second

// starter builds the start runner from the current configuration.
func (h *Instances) starter() *control.Starter {
	return &control.Starter{
		DB: h.DB, Engine: h.Engine, Runtime: h.Runtime, Keeper: h.Keeper,
		HostRoot: h.Cfg.Data.HostRoot, Image: h.Cfg.Game.Image, Network: h.Cfg.Game.Network,
		StopTimeout: h.Cfg.Game.StopTimeout.Std(), ReadySettle: h.Cfg.Jobs.ReadySettle.Std(),
		ReadyTimeout: h.Cfg.Jobs.ReadyTimeout.Std(), PluginLoadWindow: pluginLoadWindow,
	}
}

// stopper builds the stop runner from the current configuration.
func (h *Instances) stopper() *control.Stopper {
	return &control.Stopper{
		Engine: h.Engine, Runtime: h.Runtime,
		StopTimeout: h.Cfg.Game.StopTimeout.Std(), ReadyTimeout: h.Cfg.Jobs.ReadyTimeout.Std(),
	}
}

// stop is POST /instances/{id}/stop (04 §3, ADR-028): graceful SIGINT, drain timeout.
func (h *Instances) stop(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceStop, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindStop) {
		return
	}
	containerID, ok := h.mustHaveContainer(w, r, inst)
	if !ok {
		return
	}

	job, err := h.stopper().Submit(r.Context(), &control.StopSubmission{
		Instance: inst, ContainerID: containerID, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.stop", struct{}{}),
	})
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// restart is POST /instances/{id}/restart (04 §3, ADR-028): `stopping`→`starting` as one
// job (12 §2.2 — see internal/instance/state.go).
func (h *Instances) restart(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceRestart, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindRestart) {
		return
	}
	containerID, ok := h.mustHaveContainer(w, r, inst)
	if !ok {
		return
	}

	job, err := h.submitRestart(r.Context(), inst, containerID, u.ID, "")
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitRestart shares the domain submission path with scheduled restarts.
func (h *Instances) submitRestart(
	ctx context.Context, inst *store.Instance, containerID, requestedBy, scheduleID string,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.restarter().Submit(ctx, &control.RestartSubmission{
		Instance: inst, ContainerID: containerID, RequestedBy: requestedBy, ScheduleID: scheduleID,
		Audit: jobAudit(ctx, requestedBy, inst.ID, "instances.restart", struct{}{}),
	})
}

func (h *Instances) restarter() *control.Restarter {
	return &control.Restarter{
		Engine:  h.Engine,
		Starter: *h.starter(),
		Stopper: *h.stopper(),
		Archive: (&control.Backupper{DB: h.DB, DataRoot: h.Cfg.Data.Root}).ArchiveOnRestart,
	}
}

// parseKeepWorlds reads DELETE /instances/{id}'s one query parameter (04 §3). Absent
// defaults to true (12 §10): the panel never removes worlds/ unless told to.
func parseKeepWorlds(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("keep_worlds")
	if raw == "" {
		return true, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apierr.New(errcode.InvalidParameter).With("parameter", "keep_worlds").Wrap(err)
	}
	return v, nil
}

// delete is DELETE /instances/{id} (04 §3, ADR-028): from `stopped` or `error` only.
func (h *Instances) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceDelete, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	keepWorlds, err := parseKeepWorlds(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindDelete) {
		return
	}
	job, err := h.submitDelete(r.Context(), inst, keepWorlds, u.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitDelete claims `<current> → deleting` and dispatches the delete job. from is the
// instance's own state: `stopped` or `error` for the endpoint, and `deleting` for a re-run of a
// delete whose process died (12 §9.2), which the compare-and-swap accepts as a
// self-transition.
func (h *Instances) submitDelete(
	ctx context.Context, inst *store.Instance, keepWorlds bool, requestedBy string,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return (&control.Deleter{
		Engine: h.Engine, Runtime: h.Runtime, DataRoot: h.Cfg.Data.Root, RemoveAll: h.removeAll,
	}).Submit(ctx, &control.DeleteSubmission{
		Instance: inst, KeepWorlds: keepWorlds, RequestedBy: requestedBy,
		Audit: jobAudit(ctx, requestedBy, inst.ID, "instances.delete", control.DeletePayload{KeepWorlds: keepWorlds}),
	})
}

// mustLoadInstance reads id, answering 404 on both a read failure and a missing row — the
// same envelope get() already uses.
func (h *Instances) mustLoadInstance(w http.ResponseWriter, r *http.Request, id string) (*store.Instance, bool) {
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, false
	}
	return inst, true
}

// mustHaveContainer defends a data-integrity invariant rather than a client mistake: every
// instance reachable from `stopped` or `running` has already been through a successful
// provision (12 §2.2), which always sets container_id.
func (h *Instances) mustHaveContainer(w http.ResponseWriter, r *http.Request, inst *store.Instance) (string, bool) {
	if inst.ContainerID == nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).
			Wrap(fmt.Errorf("instance %s in state %s has no container_id", inst.ID, inst.State)))
		return "", false
	}
	return *inst.ContainerID, true
}
