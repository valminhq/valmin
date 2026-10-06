package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
)

// moddedConfirmationMessage is what control.ErrModdedNotConfirmed reads as to the person who has to
// answer it. 09 §3 puts the consequence next to the choice, in these terms.
const moddedConfirmationMessage = "This instance has mods installed. A game update replaces " +
	"the server files, and the installed mods may not load against the new build. Confirm to continue."

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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceUpdate, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	var body control.GameUpdatePayload
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// Checked before the submit, so an unconfirmed modded update creates no job row at all
	// rather than one that fails a second later.
	if err := control.ConfirmModded(inst, body.ConfirmModded); err != nil {
		if errors.Is(err, control.ErrModdedNotConfirmed) {
			writeFieldError(w, r, "confirm_modded", apierr.FieldRequired, moddedConfirmationMessage)
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	job, err := h.submitGameUpdate(r.Context(), inst, body.ConfirmModded, u.ID, "")
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitGameUpdate shares the domain submission path with scheduled updates.
func (h *Instances) submitGameUpdate(
	ctx context.Context, inst *store.Instance, confirmed bool, requestedBy, scheduleID string,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.gameUpdater().Submit(ctx, &control.GameUpdateSubmission{
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
		Engine: h.Engine, Runtime: h.Runtime, Config: h.Cfg,
		Snapshotter: h.snapshotter(), StageReplay: replay,
	}
}

// assertStopped asks Docker whether the server is down, for a job about to read or replace a
// tree the server writes. The state column said `stopped` when the lock was taken, and the lock
// keeps the panel out; it does not keep out `unless-stopped` after a host reboot or an operator
// with a docker CLI, and renaming a tree out from under a running server is unrecoverable.
// Never call it inside a write transaction: it is a Docker round trip.
func (h *Instances) assertStopped(ctx context.Context, inst *store.Instance) error {
	return control.AssertStopped(ctx, h.Runtime, inst) //nolint:wrapcheck // preserve the job error text
}
