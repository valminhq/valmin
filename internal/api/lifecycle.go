package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// lifecycleLogTailLines is 12 §2.2's "last N log lines attached" for a start that never
// becomes ready.
const lifecycleLogTailLines = 50

// checkInstanceState is 12 §3.1's "Requires" column, checked before a job is even
// submitted: a client gets 409 invalid_state with allowed_states populated rather than
// racing the job engine's own compare-and-swap inside OnClaim for what should be the common
// case, not an edge one.
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
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		apierr.Write(w, r, apierr.New(apierr.JobInProgress).With("job_id", conflict.JobID))
		return
	}
	apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
}

// finishToError is the OnFinish shared by every lifecycle failure that parks the instance:
// the state flip from wherever the job was running to `error`, written from data already in
// memory (12 §6's corollary).
func finishToError(instanceID string, from instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := store.TxUpdateInstanceState(
			ctx,
			tx,
			instanceID,
			string(from),
			string(instance.StateError),
		); err != nil {
			return fmt.Errorf("park instance %s in error: %w", instanceID, err)
		}
		return nil
	}
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
	containerID, ok := h.mustHaveContainer(w, r, inst)
	if !ok {
		return
	}

	job, err := h.submitStart(r.Context(), inst, containerID, u.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitStart claims `stopped → starting` and dispatches the start job. Three callers reach
// it: POST /instances/{id}/start, ADR-033's start_after_provision, and 12 §9.3's resume
// intent — all three enter `starting` through the identical claim rather than three
// hand-rolled ones that could drift.
func (h *Instances) submitStart(
	ctx context.Context, inst *store.Instance, containerID, requestedBy string,
) (*store.Job, error) {
	id := inst.ID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindStart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: struct{}{},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateStarting))
			if err != nil {
				return fmt.Errorf("claim start for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in stopped state at claim", id)
			}
			return nil
		},
	}, h.runStart(id, containerID))
	if err != nil {
		return nil, fmt.Errorf("submit start for instance %s: %w", id, err)
	}
	return job, nil
}

// runStart is the start job's Runner (12 §6): start the container, then wait for readiness.
func (h *Instances) runStart(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 20, "starting container")
		return h.startAndAwaitReady(ctx, jh, instanceID, containerID)
	}
}

// startAndAwaitReady is the start-container-then-await-readiness-then-finish sequence
// shared by `start` and `restart`'s own internal continuation (12 §3.1): both enter
// `starting` and resolve to `running` (with or without ADR-043's warning) or `error`
// (12 §3.3) via the identical shape.
func (h *Instances) startAndAwaitReady(
	ctx context.Context,
	jh *jobs.Handle,
	instanceID, containerID string,
) jobs.Outcome {
	if err := h.Runtime.Start(ctx, containerID); err != nil {
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error:    fmt.Sprintf("start container: %v", err),
			OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}

	jh.Progress(ctx, 60, "waiting for readiness")
	confirmed, err := instance.AwaitReady(
		ctx, h.Runtime, containerID, h.Cfg.Jobs.ReadySettle.Std(), h.Cfg.Jobs.ReadyTimeout.Std())
	if err != nil {
		if tail, tailErr := instance.LogTail(ctx, h.Runtime, containerID, lifecycleLogTailLines); tailErr == nil {
			jh.Log(tail)
		}
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error:    fmt.Sprintf("the server did not become ready: %v", err),
			OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}

	msg := "running"
	if !confirmed {
		// ADR-043: absence of the readiness line is a warning on a running instance, not a
		// failure — the backend acknowledgement is unconfirmed, not the server's health.
		msg = "running (registration unconfirmed)"
	}
	if !h.assertPluginsLoaded(ctx, jh, instanceID, containerID) {
		msg += "; BepInEx did not report loading any plugins"
	}
	jh.Progress(ctx, 100, msg)
	return jobs.Outcome{
		Status: "succeeded",
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return store.TxFinishStart(
				ctx,
				tx,
				instanceID,
				string(instance.StateStarting),
				string(instance.StateRunning),
			)
		},
	}
}

// pluginLoadWindow is how long a modded server gets to announce its plugin count after it
// is otherwise ready. BepInEx's chainloader runs during preload, *before* the game reaches
// the readiness line, so by this point the line has either been printed or never will be —
// this is slack for a loaded host, not a wait for something still in progress.
// A var, not a const, only so a test can shrink it rather than waiting out five real
// seconds to prove that an absent line is eventually reported.
var pluginLoadWindow = 5 * time.Second

// assertPluginsLoaded is mandatory rather than nice to have (E1). A modded instance that
// reaches `running` with no BepInEx plugin-count line is the measured silent-failure shape
// — boots, logs nothing, loads nothing — so the panel says so out loud instead of
// reporting a clean start.
//
// It returns a bool and never an error, and the instance stays `running` either way. A
// vanilla instance is not asked the question at all.
func (h *Instances) assertPluginsLoaded(
	ctx context.Context, jh *jobs.Handle, instanceID, containerID string,
) bool {
	inst, err := h.DB.InstanceByID(ctx, instanceID)
	if err != nil || inst == nil || !inst.Modded {
		return true
	}
	if instance.AwaitPluginLoad(ctx, h.Runtime, containerID, pluginLoadWindow) {
		return true
	}
	jh.Log("warning: this server is modded, but BepInEx never reported a plugin count. " +
		"The server is running and will keep running; it is probably running vanilla. " +
		"Check that BepInEx is installed under server/ and that [Logging.Console] Enabled is true.")
	slog.WarnContext(ctx, "modded instance started without a BepInEx plugin-count line",
		slog.String("instance_id", instanceID), slog.String("container_id", containerID))
	return false
}

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
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateRunning), string(instance.StateStopping))
			if err != nil {
				return fmt.Errorf("claim stop for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, h.runStop(id, containerID))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// runStop is the stop job's Runner (12 §6, 12 §3.4).
func (h *Instances) runStop(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 30, "stopping container")
		clean, timedOut, err := h.stopContainer(ctx, containerID)
		if err != nil {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(), Error: err.Error(),
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		if timedOut {
			jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error:    "the server did not stop within the timeout and was force-killed",
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}

		msg := "stopped"
		if !clean {
			msg = "stopped (world save not confirmed)"
		}
		jh.Progress(ctx, 100, msg)
		cleanCopy := clean
		return jobs.Outcome{
			Status: "succeeded",
			Clean:  &cleanCopy,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				ok, err := store.TxUpdateInstanceState(
					ctx, tx, instanceID, string(instance.StateStopping), string(instance.StateStopped))
				if err != nil {
					return fmt.Errorf("finish stop for instance %s: %w", instanceID, err)
				}
				if !ok {
					return fmt.Errorf("finish stop for instance %s: not in stopping state", instanceID)
				}
				return nil
			},
		}
	}
}

// stopContainer sends SIGINT and waits (12 §3.4), reporting whether the save-complete line
// was seen and whether Docker had to escalate to SIGKILL. Shared by the stop job and
// restart's own internal stop phase.
//
// Docker's ContainerStop already runs the signal-then-escalate sequence and blocks
// until the container is gone; the API names no field for which path was taken, so elapsed
// wall time against the same timeout is the boring, measured-enough proxy: graceful stops
// were measured at 3-5 s against a 120 s floor, so the two cases are nowhere close to each
// other.
func (h *Instances) stopContainer(ctx context.Context, containerID string) (clean, timedOut bool, err error) {
	timeout := h.Cfg.Game.StopTimeout.Std()
	start := time.Now()
	if err := h.Runtime.Stop(ctx, containerID, "SIGINT", timeout); err != nil {
		return false, false, fmt.Errorf("stop container: %w", err)
	}
	if time.Since(start) >= timeout {
		return false, true, nil
	}

	seenClean, saveErr := instance.SawSaveLine(ctx, h.Runtime, containerID)
	if saveErr != nil {
		//nolint:nilerr // deliberate: the container did stop, so the job still succeeds; an
		// unreadable log just means the save cannot be claimed clean.
		return false, false, nil
	}
	return seenClean, false, nil
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

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindRestart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: struct{}{},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateRunning), string(instance.StateStopping))
			if err != nil {
				return fmt.Errorf("claim restart for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, h.runRestart(id, containerID))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// runRestart is the restart job's Runner: stop, then continue into the same
// start-and-await-readiness sequence `start` uses, all under the one lock (ADR-028).
func (h *Instances) runRestart(instanceID, containerID string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "stopping container")
		clean, timedOut, err := h.stopContainer(ctx, containerID)
		cleanCopy := clean
		if err != nil {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(), Error: err.Error(),
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		if timedOut {
			jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error:    "the server did not stop within the timeout and was force-killed",
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}

		// restart's own internal continuation (12 §3.1) — not a client claiming
		// `start`, so a plain autocommit write rather than a second Submit. 12 §9.4 lists
		// start/stop/restart as having no checkpoints to resume from; a crash landing here
		// parks the instance in `stopping` for crash recovery to resolve, an accepted gap
		// rather than a checkpoint invented for a job kind that has none.
		if _, err := h.DB.UpdateInstanceState(
			ctx, instanceID, string(instance.StateStopping), string(instance.StateStarting)); err != nil {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error: fmt.Sprintf("move instance %s to starting: %v", instanceID, err), Clean: &cleanCopy,
			}
		}

		jh.Progress(ctx, 50, "starting container")
		outcome := h.startAndAwaitReady(ctx, jh, instanceID, containerID)
		outcome.Clean = &cleanCopy
		return outcome
	}
}

// deletePayload carries keep_worlds. A crash-recovery re-run of an interrupted delete
// needs to know it, so it travels on the job row rather than only in the request that
// started it.
type deletePayload struct {
	KeepWorlds bool `json:"keep_worlds"`
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
// instance's own state, which is `stopped` or `error` for the endpoint and `deleting` for
// 12 §9.2's re-run of a delete whose process died — a self-transition the compare-and-swap
// accepts, and the reason the re-run needs no separate claim.
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
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: deletePayload{KeepWorlds: keepWorlds},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(ctx, tx, id, from, string(instance.StateDeleting))
			if err != nil {
				return fmt.Errorf("claim delete for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in %s state at claim", id, from)
			}
			return nil
		},
	}, h.runDelete(id, containerID, inst.DataDir, keepWorlds))
	if err != nil {
		return nil, fmt.Errorf("submit delete for instance %s: %w", id, err)
	}
	return job, nil
}

// runDelete is the delete job's Runner (12 §6, 12 §9.4): idempotent throughout, since a
// live failure here leaves the instance parked in `deleting` for a future retry rather than
// inventing an `error` edge the transition table does not give this state (12 §2.1 — its
// only documented successor is the row not existing at all).
func (h *Instances) runDelete(instanceID, containerID, dataDir string, keepWorlds bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "removing container")
		if containerID != "" {
			if err := h.Runtime.Remove(ctx, containerID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return jobs.Outcome{
					Status: "failed", ErrorCode: apierr.Internal.String(),
					Error: fmt.Sprintf("remove container: %v", err),
				}
			}
		}

		jh.Progress(ctx, 60, "removing files")
		// The only recursive delete in the panel, so its target is checked against the
		// configured root before anything is unlinked (B5). data_dir is panel-generated —
		// host root plus a UUIDv7 — and no user string ever reaches the column, which is
		// exactly why an unexpected value here means something is wrong enough to stop for.
		root := filepath.Clean(h.Cfg.Data.HostRoot) + "/instances/"
		dir := filepath.Clean(dataDir)
		if !strings.HasPrefix(dir, root) || strings.Contains(dir, "..") {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error: fmt.Sprintf("refusing to remove %s: not under %s", dataDir, root),
			}
		}
		// worlds/ survives unless keep_worlds is false (12 §10) — the panel never
		// removes it outside this one path. server/ and logs/ are always disposable
		// (B3, 08 §4.1) and are reclaimed either way.
		if keepWorlds {
			for _, sub := range []string{"server", "logs"} {
				if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
					jh.Log(fmt.Sprintf("remove %s/%s: %v", dir, sub, err))
				}
			}
		} else if err := os.RemoveAll(dir); err != nil {
			jh.Log(fmt.Sprintf("remove %s: %v", dir, err))
		}

		jh.Progress(ctx, 100, "deleted")
		return jobs.Outcome{
			Status: "succeeded",
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxDeleteInstance(ctx, tx, instanceID, string(instance.StateDeleting))
			},
		}
	}
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
