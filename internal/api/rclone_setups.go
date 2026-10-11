package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup/rclonesetup"
	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/backup/remotecopy"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
)

func rcloneSetupRoutes(rt *routeTable, h *RemoteBackups) {
	rt.Handle("POST /api/v1/admin/rclone-setups", http.HandlerFunc(h.startRcloneSetup))
	rt.Handle("GET /api/v1/admin/rclone-setups/{id}", http.HandlerFunc(h.getRcloneSetup))
	rt.Handle("POST /api/v1/admin/rclone-setups/{id}/advance", http.HandlerFunc(h.advanceRcloneSetup))
	rt.Handle("DELETE /api/v1/admin/rclone-setups/{id}", http.HandlerFunc(h.cancelRcloneSetup))
	rt.Handle("POST /api/v1/admin/rclone-setups/{id}/finish", http.HandlerFunc(h.finishRcloneSetup))
}

// startRcloneSetup opens a guided setup for a new rclone connection, owned by the caller's
// login session.
func (h *RemoteBackups) startRcloneSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body rclonesetup.Request
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	callback := strings.TrimRight(h.Cfg.Server.ExternalURL, "/") + rclonesetup.CallbackPath
	status, err := h.Setups.Start(r.Context(), middleware.SessionIDFrom(r.Context()), callback, &body)
	if err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	JSON(w, r, http.StatusCreated, status)
}

func (h *RemoteBackups) getRcloneSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	status, err := h.Setups.Status(middleware.SessionIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, status)
}

type rcloneSetupAdvance struct {
	State   string `json:"state"`
	Code    string `json:"code"`
	Error   string `json:"error"`
	DriveID string `json:"drive_id"`
}

// advanceRcloneSetup takes the provider's authorization callback or the chosen OneDrive drive.
func (h *RemoteBackups) advanceRcloneSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body rcloneSetupAdvance
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	owner, id := middleware.SessionIDFrom(r.Context()), r.PathValue("id")
	var (
		status rclonesetup.Status
		err    error
	)
	if body.DriveID != "" {
		status, err = h.Setups.ChooseDrive(owner, id, body.DriveID)
	} else {
		status, err = h.Setups.Authorize(r.Context(), owner, id, rclonesetup.Callback{
			State: body.State, Code: body.Code, Error: body.Error,
		})
	}
	if err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, status)
}

func (h *RemoteBackups) cancelRcloneSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if err := h.Setups.Cancel(middleware.SessionIDFrom(r.Context()), r.PathValue("id")); err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// finishRcloneSetup submits the job that writes a ready setup's remote to rclone.conf. The
// credentials stay in the job's runner; the payload and audit carry only name and provider.
func (h *RemoteBackups) finishRcloneSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	owner, id := middleware.SessionIDFrom(r.Context()), r.PathValue("id")
	status, err := h.Setups.Status(owner, id)
	if err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	section, err := h.Setups.Section(owner, id)
	if err != nil {
		rcloneSetupError(w, r, err)
		return
	}
	detail := map[string]string{"name": status.Name, "provider": status.Provider}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind:        jobs.KindRemoteCreate,
		LockKey:     remotecopy.LockKey,
		RequestedBy: u.ID,
		Payload:     detail,
		Audit:       jobAudit(r.Context(), u.ID, "", "remote_backups.connection.create", detail),
	}, h.createRemote(owner, id, section))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// createRemote appends section to rclone.conf and closes the setup once it is written, so a
// failed attempt can be retried until the setup expires.
func (h *RemoteBackups) createRemote(owner, id string, section remote.RcloneSection) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 50, "Saving the rclone connection")
		err := h.rclone().AddRemote(section)
		if errors.Is(err, remote.ErrRemoteExists) {
			return jobs.Outcome{
				Status:    jobs.StatusFailed,
				ErrorCode: errcode.NameTaken.String(),
				Error:     "An rclone connection with this name already exists.",
			}
		}
		if failure, ok := errors.AsType[*remote.Failure](err); ok {
			return remotecopy.Failed(failure.Message)
		}
		if err != nil {
			slog.ErrorContext(ctx, "save rclone connection", slog.Any("error", err))
			return remotecopy.Failed("The rclone connection could not be saved.")
		}
		// The setup may already have expired; the connection is saved either way.
		_ = h.Setups.Cancel(owner, id)
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

func rcloneSetupError(w http.ResponseWriter, r *http.Request, err error) {
	if fields, ok := errors.AsType[rclonesetup.ValidationError](err); ok {
		var v apierr.Validation
		for _, f := range fields {
			code := apierr.FieldInvalid
			if f.Required {
				code = apierr.FieldRequired
			}
			v.Add(f.Field, code, f.Message)
		}
		apierr.Write(w, r, v.Err())
		return
	}
	if authErr, ok := errors.AsType[*rclonesetup.AuthorizationError](err); ok {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).Msg(authErr.Message))
		return
	}
	switch {
	case errors.Is(err, remote.ErrRemoteExists):
		apierr.Write(w, r, apierr.New(errcode.NameTaken).
			Msg("An rclone connection with this name already exists. Choose a different name."))
	case errors.Is(err, rclonesetup.ErrNotFound):
		apierr.Write(w, r, apierr.New(errcode.NotFound).
			Msg("This connection setup expired or was cancelled. Start again."))
	case errors.Is(err, rclonesetup.ErrStaleState):
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
			Msg("This authorization response is out of date. Connect the account again."))
	case errors.Is(err, rclonesetup.ErrWrongStep):
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
			Msg("Finish the current step of the connection setup first."))
	default:
		remoteAPIError(w, r, err)
	}
}
