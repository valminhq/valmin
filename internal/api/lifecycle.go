package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
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
	apierr.Write(w, r, apierr.New(apierr.InvalidState).With("state", inst.State).With("allowed_states", allowed))
	return false
}

// writeJobSubmitError is the ADR-030 shape every job-creating endpoint answers with: a lock
// collision is 409 job_in_progress naming the active job, never a queued placeholder.
func writeJobSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrBackupProtected) {
		apierr.Write(
			w,
			r,
			apierr.New(apierr.InvalidState).Msg("Cancel pending remote uploads before deleting this server."),
		)
		return
	}
	if errors.Is(err, jobs.ErrShuttingDown) {
		apierr.Write(w, r, apierr.New(apierr.Unavailable))
		return
	}
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		apierr.Write(w, r, apierr.New(apierr.JobInProgress).With("job_id", conflict.JobID))
		return
	}
	apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceStart, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
	id := inst.ID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindStart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: struct{}{}, Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.SetStateTx(ctx, tx, id, instance.StateStopped, instance.StateStarting)
			if err != nil {
				return fmt.Errorf("claim start for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in stopped state at claim", id)
			}
			return nil
		},
	}, (&control.Starter{DB: h.DB, Runtime: h.Runtime, Keeper: h.Keeper, HostRoot: h.Cfg.Data.HostRoot, Image: h.Cfg.Game.Image, Network: h.Cfg.Game.Network, StopTimeout: h.Cfg.Game.StopTimeout.Std(), ReadySettle: h.Cfg.Jobs.ReadySettle.Std(), ReadyTimeout: h.Cfg.Jobs.ReadyTimeout.Std(), PluginLoadWindow: pluginLoadWindow}).Run(id, containerID))
	if err != nil {
		return nil, fmt.Errorf("submit start for instance %s: %w", id, err)
	}
	return job, nil
}

// pluginLoadWindow bounds the optional plugin-count evidence after readiness.
var pluginLoadWindow = 5 * time.Second

// stop is POST /instances/{id}/stop (04 §3, ADR-028): graceful SIGINT, drain timeout.
func (h *Instances) stop(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceStop, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindStop, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: struct{}{},
		Audit:   jobAudit(r.Context(), u.ID, id, "instances.stop", struct{}{}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.SetStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
			if err != nil {
				return fmt.Errorf("claim stop for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, (control.Stopper{Runtime: h.Runtime, StopTimeout: h.Cfg.Game.StopTimeout.Std(), ReadyTimeout: h.Cfg.Jobs.ReadyTimeout.Std()}).Run(id, containerID))
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceRestart, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
	return h.restarter().Submit(ctx, h.Engine, control.RestartSubmission{
		Instance: inst, ContainerID: containerID, RequestedBy: requestedBy, ScheduleID: scheduleID,
		Audit: jobAudit(ctx, requestedBy, inst.ID, "instances.restart", struct{}{}),
	})
}

func (h *Instances) restarter() *control.Restarter {
	return &control.Restarter{
		Starter: control.Starter{
			DB:               h.DB,
			Runtime:          h.Runtime,
			Keeper:           h.Keeper,
			HostRoot:         h.Cfg.Data.HostRoot,
			Image:            h.Cfg.Game.Image,
			Network:          h.Cfg.Game.Network,
			StopTimeout:      h.Cfg.Game.StopTimeout.Std(),
			ReadySettle:      h.Cfg.Jobs.ReadySettle.Std(),
			ReadyTimeout:     h.Cfg.Jobs.ReadyTimeout.Std(),
			PluginLoadWindow: pluginLoadWindow,
		},
		Stopper: control.Stopper{
			Runtime:      h.Runtime,
			StopTimeout:  h.Cfg.Game.StopTimeout.Std(),
			ReadyTimeout: h.Cfg.Jobs.ReadyTimeout.Std(),
		},
		Archive: (&control.Backupper{DB: h.DB, DataRoot: h.Cfg.Data.Root}).ArchiveOnRestart,
	}
}

// deletePayload carries keep_worlds. A crash-recovery re-run of an interrupted delete
// needs to know it, so it travels on the job row rather than only in the request that
// started it.
type deletePayload = control.DeletePayload

// parseKeepWorlds reads DELETE /instances/{id}'s one query parameter (04 §3). Absent
// defaults to true (12 §10): the panel never removes worlds/ unless told to.
func parseKeepWorlds(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("keep_worlds")
	if raw == "" {
		return true, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apierr.New(apierr.InvalidParameter).With("parameter", "keep_worlds").Wrap(err)
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceDelete, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
	id, from := inst.ID, inst.State
	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}

	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindDelete, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{"remote_instance:" + id},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: deletePayload{KeepWorlds: keepWorlds},
		Audit:   jobAudit(ctx, requestedBy, id, "instances.delete", deletePayload{KeepWorlds: keepWorlds}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			var ok bool
			var err error
			if err := store.TxCheckRemoteProtection(ctx, tx, id, ""); err != nil {
				return fmt.Errorf("remote operation: %w", err)
			}
			if instance.State(from) == instance.StateDeleting {
				ok, err = instance.HoldStateTx(ctx, tx, id, instance.StateDeleting)
			} else {
				ok, err = instance.SetStateTx(ctx, tx, id, instance.State(from), instance.StateDeleting)
			}
			if err != nil {
				return fmt.Errorf("claim delete for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in %s state at claim", id, from)
			}
			return nil
		},
	}, (control.Deleter{Runtime: h.Runtime, DataRoot: h.Cfg.Data.Root, RemoveAll: h.removeAll}).Run(id, containerID, inst.DataDir, keepWorlds))
	if err != nil {
		return nil, fmt.Errorf("submit delete for instance %s: %w", id, err)
	}
	return job, nil
}

// mustLoadInstance reads id, answering 404 on both a read failure and a missing row — the
// same envelope get() already uses.
func (h *Instances) mustLoadInstance(w http.ResponseWriter, r *http.Request, id string) (*store.Instance, bool) {
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, false
	}
	return inst, true
}

// mustHaveContainer defends a data-integrity invariant rather than a client mistake: every
// instance reachable from `stopped` or `running` has already been through a successful
// provision (12 §2.2), which always sets container_id.
func (h *Instances) mustHaveContainer(w http.ResponseWriter, r *http.Request, inst *store.Instance) (string, bool) {
	if inst.ContainerID == nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).
			Wrap(fmt.Errorf("instance %s in state %s has no container_id", inst.ID, inst.State)))
		return "", false
	}
	return *inst.ContainerID, true
}
