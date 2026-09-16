package api

import (
	"encoding/json"
	"errors"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/store"
)

type commandRequest struct {
	Command string `json:"command"`
}

type commandResponse struct {
	Accepted bool   `json:"accepted"`
	Output   string `json:"output"`
}

func (h *Instances) command(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.CommandsSend, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
	var body commandRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	detail, err := json.Marshal(map[string]string{"channel": "rcon", "command": body.Command})
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.commands.send", Detail: string(detail),
		IP: middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	output, err := h.Commands.Send(r.Context(), inst, body.Command, u.Role == store.RoleAdmin)
	if err != nil {
		writeCommandError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, commandResponse{Accepted: true, Output: output})
}

func writeCommandError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, command.ErrUnsupported):
		apierr.Write(w, r, apierr.New(apierr.Unsupported).Wrap(err))
	case errors.Is(err, command.ErrInvalidState):
		apierr.Write(w, r, apierr.New(apierr.InvalidState).Wrap(err))
	case errors.Is(err, command.ErrInvalidCommand), errors.Is(err, command.ErrCommandForbidden):
		apierr.Write(w, r, apierr.New(apierr.ValidationFailed).With("field", "command").Wrap(err))
	case errors.Is(err, command.ErrRateLimited):
		apierr.Write(w, r, apierr.New(apierr.RateLimited).Wrap(err))
	default:
		apierr.Write(w, r, apierr.New(apierr.Unavailable).Wrap(err))
	}
}
