package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
)

type updateStatusView struct {
	InstalledBuildID *string    `json:"installed_build_id"`
	PublicBuildID    *string    `json:"public_build_id"`
	ObservedAt       *time.Time `json:"observed_at"`
	UpdateAvailable  *bool      `json:"update_available"`
}

// updateStatus reports the last successful observation; failed checks never replace it.
func (h *Instances) updateStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	var observed instance.PublicBuild
	found, err := h.DB.KVGet(r.Context(), instance.PublicBuildKey, &observed)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	v := updateStatusView{}
	// The manifest under server/ is what the instance actually runs; the column is a cache of
	// it, and a game update whose recovery completed the swap without reaching its Finish
	// transaction leaves the two disagreeing. Legacy rows carry a cache alias rather than a
	// version, which the column could not answer either way.
	installed, err := instance.InstalledBuildID(inst.DataDir)
	if err != nil {
		installed = deref(inst.GameBuildID)
	}
	if knownBuildID(installed) {
		v.InstalledBuildID = &installed
	}
	if found && knownBuildID(observed.BuildID) && !observed.ObservedAt.IsZero() {
		v.PublicBuildID, v.ObservedAt = &observed.BuildID, &observed.ObservedAt
		if v.InstalledBuildID != nil {
			available := installed != observed.BuildID
			v.UpdateAvailable = &available
		}
	}
	JSON(w, r, http.StatusOK, v)
}

func knownBuildID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0
}

// newBuildNotification owes an update-available notification when the build just observed is
// not the one already recorded. Nil when there is nothing to say, or no notifier wired.
func (h *Instances) newBuildNotification(
	ctx context.Context, observed string,
) func(context.Context, *sql.Tx) error {
	if h.Notify == nil {
		return nil
	}
	var previous instance.PublicBuild
	if _, err := h.DB.KVGet(ctx, instance.PublicBuildKey, &previous); err != nil {
		slog.WarnContext(ctx, "read the last observed build", slog.Any("error", err))
		return nil
	}
	return h.Notify.NotifyPublicBuild(ctx, previous.BuildID, observed)
}

func (h *Instances) updateChecker() *control.UpdateChecker {
	return (&control.UpdateChecker{
		DB: h.DB, Engine: h.Engine, Runtime: h.Runtime, Config: h.Cfg,
		NewBuildNotification: h.newBuildNotification,
	})
}
