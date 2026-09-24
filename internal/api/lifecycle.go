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
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// lifecycleLogTailLines is 12 §2.2's "last N log lines attached" for a start that never
// becomes ready.
const lifecycleLogTailLines = 50

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

// finishToError is the OnFinish shared by every lifecycle failure that parks the instance:
// the state flip from wherever the job was running to `error`, written from data already in
// memory (12 §6's corollary).
func finishToError(instanceID string, from instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := setStateTx(ctx, tx, instanceID, from, instance.StateError); err != nil {
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
	if !operationSettled(w, r, h.DB, id) {
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

// submitStart claims `stopped → starting` and dispatches the start job. Its three callers are
// POST /instances/{id}/start, start_after_provision (ADR-033) and a resume intent (12 §9.3), so
// all of them enter `starting` through one claim.
func (h *Instances) submitStart(
	ctx context.Context, inst *store.Instance, containerID, requestedBy string,
) (*store.Job, error) {
	id := inst.ID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindStart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: struct{}{},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := setStateTx(ctx, tx, id, instance.StateStopped, instance.StateStarting)
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

// startAndAwaitReady starts the container, awaits readiness and finishes the job. Shared by
// `start` and `restart`'s internal continuation, which both enter `starting` and resolve to
// `running`, with or without ADR-043's warning, or `error` (12 §3.1, 12 §3.3).
func (h *Instances) startAndAwaitReady(
	ctx context.Context,
	jh *jobs.Handle,
	instanceID, containerID string,
) jobs.Outcome {
	// Both start and restart arrive here, so one check applies edited settings on either
	// path.
	containerID, err := h.rebuildIfDrifted(ctx, jh, instanceID, containerID)
	if err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
			Error:    err.Error(),
			OnFinish: finishToError(instanceID, instance.StateStarting),
		}
	}

	if err := h.Runtime.Start(ctx, containerID); err != nil {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
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
			Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
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
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return finishStartState(
				ctx, tx, instanceID, instance.StateStarting, instance.StateRunning,
			)
		},
	}
}

// pluginLoadWindow is how long a modded server gets to announce its plugin count after it is
// otherwise ready. The chainloader runs during preload, before the readiness line, so the line
// has already been printed or never will be: this is slack for a loaded host. A var so a test
// can shrink it.
var pluginLoadWindow = 5 * time.Second

// assertPluginsLoaded reports whether a modded instance announced its plugin count. A modded
// server reaching `running` without that line is the measured silent-failure shape: it boots,
// logs nothing and loads nothing (E1).
//
// It returns a bool and never an error, and the instance stays `running` either way. A vanilla
// instance is not asked the question.
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
			ok, err := setStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
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
				Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(), Error: err.Error(),
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		if timedOut {
			jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
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
			Status: jobs.StatusSucceeded,
			Clean:  &cleanCopy,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				ok, err := setStateTx(ctx, tx, instanceID, instance.StateStopping, instance.StateStopped)
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

// stopContainer sends SIGINT and waits (12 §3.4), reporting whether the save-complete line was
// seen and whether Docker had to escalate to SIGKILL. Shared by the stop job and restart's stop
// phase.
//
// Docker's ContainerStop runs the signal-then-escalate sequence itself and names no field for
// which path it took, so elapsed wall time against the timeout is the proxy: graceful stops
// measure 3-5 s against a 120 s floor.
func (h *Instances) stopContainer(ctx context.Context, containerID string) (clean, timedOut bool, err error) {
	h.awaitSignalHonoured(ctx, containerID)

	timeout := h.Cfg.Game.StopTimeout.Std()
	// The cursor for the save evidence, taken before the signal: only a completion line after
	// this instant belongs to this stop. The server writes that literal on every save, so a
	// boot-scoped search would accept an autosave from earlier in the session (B2).
	start := time.Now()
	if err := h.Runtime.Stop(ctx, containerID, "SIGINT", timeout); err != nil {
		return false, false, fmt.Errorf("stop container: %w", err)
	}
	if time.Since(start) >= timeout {
		return false, true, nil
	}

	seenClean, saveErr := instance.SawSaveLine(ctx, h.Runtime, containerID, start)
	if saveErr != nil {
		//nolint:nilerr // deliberate: the container did stop, so the job still succeeds; an
		// unreadable log just means the save cannot be claimed clean.
		return false, false, nil
	}
	return seenClean, false, nil
}

// awaitSignalHonoured holds a stop until the container's current boot has reached the point
// where the game acts on SIGINT.
//
// `↯` Before that point the signal does not reach the shutdown path: measured on l-1.0.12, one
// sent at the `Setting -savedir` line ended the process with exit 0, no `Game -
// OnApplicationQuit` and no save, while one sent six seconds later, at the first chunk-load
// line, saved cleanly (Q57, 03 §3.2). The world survives that window — it is not loaded yet,
// and the files were byte-identical across it — so what this protects is the case after it:
// a signal that arrives early, is lost rather than acted on, and leaves Docker to escalate to
// SIGKILL against a server that has since loaded the world and run for two minutes.
//
// The panel reaches the window without an operator doing anything unusual. `unless-stopped`
// restarts a crashed server underneath a row that still reads `running` (ADR-020), so the next
// stop signals a boot seconds old.
//
// Gated on the container's own uptime rather than on the ready line alone: a container that has
// been up longer than the readiness window is past startup whatever its log says, which is what
// keeps an adopted container, or one started on ADR-043's unconfirmed fallback, from waiting
// here for a line that is never coming.
//
// Every failure leaves the stop to proceed. A stop an operator asked for must not become
// unreachable because a server never finished booting, and Docker's stop of a container that
// has already exited is a no-op.
func (h *Instances) awaitSignalHonoured(ctx context.Context, containerID string) {
	window := h.Cfg.Jobs.ReadyTimeout.Std()
	c, err := h.Runtime.Inspect(ctx, containerID)
	if err != nil || !c.Running || c.StartedAt.IsZero() {
		return
	}
	left := window - time.Since(c.StartedAt)
	if left <= 0 {
		return
	}
	if _, err := instance.AwaitReady(ctx, h.Runtime, containerID, left, left); err != nil {
		slog.WarnContext(ctx, "stopping without confirming the server finished starting",
			slog.String("container_id", containerID), slog.Any("error", err))
	}
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

// submitRestart is the one path a restart job is created through, whether an operator asked
// for it or a schedule's tick did. requestedBy is empty for the scheduler, which writes NULL
// (12 §11).
func (h *Instances) submitRestart(
	ctx context.Context, inst *store.Instance, containerID, requestedBy, scheduleID string,
) (*store.Job, error) {
	id := inst.ID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindRestart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name,
		RequestedBy: requestedBy, ScheduleID: scheduleID,
		Payload: struct{}{},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := setStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
			if err != nil {
				return fmt.Errorf("claim restart for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, h.runRestart(inst, containerID))
	if err != nil {
		// Wrapped, not replaced: writeJobSubmitError and the scheduler's skip both reach
		// through this with errors.As to find *store.JobConflict.
		return nil, fmt.Errorf("submit restart for instance %s: %w", id, err)
	}
	return job, nil
}

// runRestart is the restart job's Runner: stop, then continue into the same
// start-and-await-readiness sequence `start` uses, all under the one lock (ADR-028).
func (h *Instances) runRestart(inst *store.Instance, containerID string) jobs.Runner {
	instanceID := inst.ID
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "stopping container")
		clean, timedOut, err := h.stopContainer(ctx, containerID)
		cleanCopy := clean
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(), Error: err.Error(),
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}
		if timedOut {
			jh.Log("stop timeout exceeded; Docker escalated to SIGKILL")
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
				Error:    "the server did not stop within the timeout and was force-killed",
				OnFinish: finishToError(instanceID, instance.StateStopping),
			}
		}

		// The world is flushed and the container is down, which is every precondition an
		// archive needs, already paid for. Opportunistic: a failure warns and the restart
		// carries on, because a restart's contract is that the server comes back.
		archived, pruneCleanup := h.archiveOnRestart(ctx, jh, inst, clean)

		// restart's internal continuation (12 §3.1), not a client claiming `start`, so a plain
		// autocommit write rather than a second Submit. These kinds have no checkpoints (12 §9.4),
		// so a crash here parks the instance in `stopping` for crash recovery to resolve.
		if _, err := instance.SetState(
			ctx, h.DB, instanceID, instance.StateStopping, instance.StateStarting); err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
				Error: fmt.Sprintf("move instance %s to starting: %v", instanceID, err), Clean: &cleanCopy,
			}
		}

		jh.Progress(ctx, 50, "starting container")
		outcome := h.startAndAwaitReady(ctx, jh, instanceID, containerID)
		outcome.Clean = &cleanCopy
		if archived != nil && outcome.Status == jobs.StatusSucceeded {
			outcome.OnFinish = chainFinish(outcome.OnFinish, archived)
			outcome.AfterFinish = chainAfterFinish(pruneCleanup, outcome.AfterFinish)
		}
		return outcome
	}
}

// chainFinish runs two Finish callbacks in one transaction, so a restart's archive row and its
// state flip commit together (12 §6).
func chainFinish(first, second func(context.Context, *sql.Tx) error) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if first != nil {
			if err := first(ctx, tx); err != nil {
				return err
			}
		}
		return second(ctx, tx)
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
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: deletePayload{KeepWorlds: keepWorlds},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			var ok bool
			var err error
			if instance.State(from) == instance.StateDeleting {
				ok, err = holdStateTx(ctx, tx, id, instance.StateDeleting)
			} else {
				ok, err = setStateTx(ctx, tx, id, instance.State(from), instance.StateDeleting)
			}
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

// runDelete is the delete job's Runner (12 §6, 12 §9.4), idempotent throughout: a failure leaves
// the instance parked in `deleting` for a retry, since the transition table gives that state no
// successor but the row ceasing to exist (12 §2.1).
func (h *Instances) runDelete(instanceID, containerID, dataDir string, keepWorlds bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "removing container")
		if containerID != "" {
			if err := h.Runtime.Remove(ctx, containerID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return jobs.Outcome{
					Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
					Error: fmt.Sprintf("remove container: %v", err),
				}
			}
		}

		jh.Progress(ctx, 60, "removing files")
		if err := h.deleteInstanceFiles(instanceID, dataDir, keepWorlds); err != nil {
			return deleteFailed(err)
		}

		jh.Progress(ctx, 100, "deleted")
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxDeleteInstance(ctx, tx, instanceID, string(instance.StateDeleting))
			},
		}
	}
}

func (h *Instances) deleteInstanceFiles(instanceID, dataDir string, keepWorlds bool) error {
	root := filepath.Join(h.Cfg.Data.Root, "instances")
	dir := filepath.Clean(dataDir)
	if !withinRoot(root, dir) {
		return fmt.Errorf("refusing path outside %s", root)
	}
	backupRoot := instance.BackupsDir(h.Cfg.Data.Root)
	backupDir := filepath.Join(backupRoot, instanceID)
	if !withinRoot(backupRoot, backupDir) {
		return fmt.Errorf("refusing backup path outside %s", backupRoot)
	}
	if !keepWorlds {
		for _, path := range []string{dir, backupDir} {
			if err := h.removeInstanceFiles(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
		return nil
	}

	// World and archive bytes survive. Everything else belongs to a disposable server tree.
	for _, path := range []string{
		instance.ServerDir(dir),
		instance.StagedServerDir(dir),
		instance.ServerDir(dir) + backup.SupersededSuffix,
		filepath.Join(dir, "logs"),
		instance.UpdateStaging(dir),
		instance.ParkedModsDir(dir),
	} {
		if err := h.removeInstanceFiles(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func (h *Instances) removeInstanceFiles(path string) error {
	if h.removeAll != nil {
		return h.removeAll(path)
	}
	if err := os.RemoveAll(path); err != nil { //nolint:gosec // deleteInstanceFiles validates each target
		return fmt.Errorf("remove tree: %w", err)
	}
	return nil
}

func deleteFailed(err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(),
		Error: err.Error(),
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
