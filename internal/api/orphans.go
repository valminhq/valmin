package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Orphan is the HTTP view of a managed container no instance row claims.
type Orphan struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	InstanceID  string `json:"instance_id"`
	BasePort    int    `json:"base_port"`
	Running     bool   `json:"running"`
}

func (h *Instances) orphans(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	found, err := control.ListOrphans(r.Context(), h.DB, h.Runtime)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	view := make([]Orphan, 0, len(found))
	for _, orphan := range found {
		view = append(view, Orphan{
			ContainerID: orphan.ContainerID, Name: orphan.Name, InstanceID: orphan.InstanceID,
			BasePort: orphan.BasePort, Running: orphan.Running,
		})
	}
	JSON(w, r, http.StatusOK, NewPage(view, nil))
}

// removeOrphan is DELETE /orphans/{container_id}: it removes a stopped managed container no
// instance row claims, for one that cannot be recovered. Its data directory is left alone.
func (h *Instances) removeOrphan(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	found, err := control.ListOrphans(r.Context(), h.DB, h.Runtime)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	id := r.PathValue("container_id")
	var orphan *control.Orphan
	for i := range found {
		if found[i].ContainerID == id {
			orphan = &found[i]
		}
	}
	if orphan == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	// A forced remove kills a running game without its save; stopping one is left to the host.
	if orphan.Running {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).With("state", "running").Msg(
			"This container is running. Stop it on the host with `docker stop --time 120 "+orphan.Name+"`, then remove it."),
		)
		return
	}

	auditID := store.NewID()
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		ID: auditID, UserID: u.ID, Action: "orphans.remove",
		Detail:  detailJSON(map[string]string{"container_name": orphan.Name, "instance_id": orphan.InstanceID}),
		Outcome: store.AuditRequested, IP: clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	err = h.Runtime.ForceRemove(r.Context(), orphan.ContainerID)
	outcome := store.AuditSucceeded
	if err != nil {
		outcome = store.AuditFailed
	}
	if outcomeErr := h.DB.SetAuditOutcome(context.WithoutCancel(r.Context()), auditID, outcome); outcomeErr != nil {
		slog.ErrorContext(r.Context(), "orphan removal audit outcome not recorded",
			slog.String("audit_id", auditID), slog.Any("error", outcomeErr))
	}
	if errors.Is(err, runtime.ErrNotFound) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
