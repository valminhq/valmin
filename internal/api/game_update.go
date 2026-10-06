package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// Checkpoints of a game_update job, in the order 12 §9.4 fixes. swap_started is the point of
// no return: past it the live server/ is being renamed, and recovery has to finish the job
// rather than discard it.
const (
	checkpointBuildCached  = control.UpdateBuildCached
	checkpointCloned       = control.UpdateCloned
	checkpointModsReplayed = control.UpdateModsReplayed
	checkpointSwapStarted  = control.UpdateSwapStarted
)

// errModdedNotConfirmed is 03 §8's rule: nothing updates a modded server without being asked
// twice. A schedule never supplies the second answer, so a tick reports it as a skip
// (ADR-137).
var errModdedNotConfirmed = control.ErrModdedNotConfirmed

// moddedConfirmationMessage is what errModdedNotConfirmed reads as to the person who has to
// answer it. 09 §3 puts the consequence next to the choice, in these terms.
const moddedConfirmationMessage = "This instance has mods installed. A game update replaces " +
	"the server files, and the installed mods may not load against the new build. Confirm to continue."

// gameUpdatePayload is the job's persisted arguments (12 §4.1).
type gameUpdatePayload = control.GameUpdatePayload

// updateGame is POST /instances/{id}/update (04 §3, 12 §3.1). It requires `stopped`, never
// stops a server itself, and never starts one afterwards: the operator starts explicitly, and
// that start is what verifies BepInEx and plugin loading (08 §7 step 6, 03 §5.3).
func (h *Instances) updateGame(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceUpdate, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	var body gameUpdatePayload
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// Checked before the submit, so an unconfirmed modded update creates no job row at all
	// rather than one that fails a second later.
	if err := confirmModded(inst, body.ConfirmModded); err != nil {
		if errors.Is(err, errModdedNotConfirmed) {
			writeFieldError(w, r, "confirm_modded", apierr.FieldRequired, moddedConfirmationMessage)
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	job, err := h.submitGameUpdate(r.Context(), inst, body.ConfirmModded, u.ID, "")
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

func confirmModded(inst *store.Instance, confirmed bool) error {
	return control.ConfirmModded(inst, confirmed) //nolint:wrapcheck // preserve the validation error
}

// submitGameUpdate shares the domain submission path with scheduled updates.
func (h *Instances) submitGameUpdate(
	ctx context.Context, inst *store.Instance, confirmed bool, requestedBy, scheduleID string,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.gameUpdater().Submit(ctx, h.Engine, control.GameUpdateSubmission{
		Instance: inst, Confirmed: confirmed, RequestedBy: requestedBy, ScheduleID: scheduleID,
		Audit: jobAudit(ctx, requestedBy, inst.ID, "instances.game.update", struct{}{}),
	})
}

func (h *Instances) gameUpdater() *control.GameUpdater {
	var replay func(context.Context, *store.Instance, string) error
	if h.Mods != nil {
		replay = h.Mods.StageReplay
	}
	return &control.GameUpdater{
		Runtime: h.Runtime, Config: h.Cfg,
		Snapshotter: h.snapshotter(), StageReplay: replay,
	}
}

// errServerRunning is assertStopped's refusal. A job reports it as instance_must_be_stopped.
var errServerRunning = control.ErrServerRunning

// assertStopped asks Docker whether the server is down, for a job about to read or replace a
// tree the server writes. The state column said `stopped` when the lock was taken, and the lock
// keeps the panel out; it does not keep out `unless-stopped` after a host reboot or an operator
// with a docker CLI, and renaming a tree out from under a running server is unrecoverable.
// Never call it inside a write transaction: it is a Docker round trip.
func (h *Instances) assertStopped(ctx context.Context, inst *store.Instance) error {
	return control.AssertStopped(ctx, h.Runtime, inst) //nolint:wrapcheck // preserve the job error text
}
