package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/backup/remotecopy"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const remoteBackupLock = remotecopy.LockKey

const (
	remoteCopyIDField = remotecopy.CopyIDField
	remoteKindField   = "kind"
)

type RemoteBackups struct {
	DB         *store.DB
	Authz      *authz.Authz
	Engine     *jobs.Engine
	Keeper     *crypto.Keeper
	Cfg        *config.Config
	BackendFor func(*store.RemoteDestination) (remote.Backend, error)
}

func (h *RemoteBackups) worker() *remotecopy.Worker {
	return &remotecopy.Worker{DB: h.DB, Engine: h.Engine, BackendFor: h.backend}
}

func remoteBackupRoutes(rt *routeTable, h *RemoteBackups) {
	rt.Handle("GET /api/v1/admin/remote-backup-destination", http.HandlerFunc(h.getDestination))
	rt.Handle("PUT /api/v1/admin/remote-backup-destination", http.HandlerFunc(h.putDestination))
	rt.Handle("POST /api/v1/admin/remote-backup-destination/test", http.HandlerFunc(h.testDestination))
	rt.Handle("GET /api/v1/admin/remote-backup-destination/remotes", http.HandlerFunc(h.remotes))
	rt.Handle("POST /api/v1/instances/{id}/backups/{bid}/remote-copy", http.HandlerFunc(h.upload))
	rt.Handle("GET /api/v1/instances/{id}/remote-copies", http.HandlerFunc(h.list))
	rt.Handle("GET /api/v1/instances/{id}/remote-copies/{copy_id}", http.HandlerFunc(h.getCopy))
	rt.Handle("POST /api/v1/instances/{id}/remote-copies/{copy_id}/retry", http.HandlerFunc(h.retry))
	rt.Handle("POST /api/v1/instances/{id}/remote-copies/{copy_id}/cancel", http.HandlerFunc(h.cancel))
}

func remoteCredentialsLocation(id string) crypto.Location {
	return crypto.Location{Table: "remote_backup_destinations", Column: "credentials", RowID: id}
}

func (h *RemoteBackups) rclone() *remote.RcloneBackend {
	binary, configPath := h.Cfg.RemoteBackups.RcloneBinary, h.Cfg.RemoteBackups.RcloneConfig
	if binary == "" {
		binary = "rclone"
	}
	if configPath == "" {
		configPath = filepath.Join(h.Cfg.Data.Root, "rclone", "rclone.conf")
	}
	return &remote.RcloneBackend{Binary: binary, ConfigFile: configPath}
}

func (h *RemoteBackups) backend(d *store.RemoteDestination) (remote.Backend, error) {
	if h.BackendFor != nil {
		return h.BackendFor(d)
	}
	switch d.Kind {
	case "webdav":
		password, err := h.Keeper.Decrypt(crypto.PurposeRemoteBackup, remoteCredentialsLocation(d.ID), d.Credentials)
		if err != nil {
			return nil, &remote.Failure{Message: "Remote credentials cannot be read; enter them again."}
		}
		b := &remote.WebDAVBackend{URL: d.Endpoint, Username: d.Username, Password: string(password), Folder: d.Folder}
		for _, raw := range h.Cfg.RemoteBackups.AllowedPrivateCIDRs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, remote.ErrConfiguration
			}
			b.AllowedPrivate = append(b.AllowedPrivate, prefix)
		}
		if err := b.Validate(); err != nil {
			return nil, fmt.Errorf("validate WebDAV: %w", err)
		}
		return b, nil
	case "rclone":
		b := h.rclone()
		b.Remote, b.Folder = d.RemoteName, d.Folder
		if err := b.Validate(); err != nil {
			return nil, fmt.Errorf("validate rclone: %w", err)
		}
		return b, nil
	default:
		return nil, remote.ErrConfiguration
	}
}

type remoteDestinationView struct {
	*store.RemoteDestination
	HasCredentials bool                `json:"has_credentials"`
	Summary        store.RemoteSummary `json:"summary"`
}

func (h *RemoteBackups) getDestination(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	d, err := h.DB.RemoteDestination(r.Context())
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	if d == nil {
		JSON(w, r, http.StatusOK, nil)
		return
	}
	summary, err := h.DB.RemoteSummary(r.Context(), "")
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, remoteDestinationView{d, d.Credentials != "", summary})
}

type remoteDestinationRequest struct {
	Kind       string  `json:"kind"`
	Enabled    bool    `json:"enabled"`
	Endpoint   string  `json:"endpoint"`
	Username   string  `json:"username"`
	Password   *string `json:"password"`
	RemoteName string  `json:"remote_name"`
	Folder     string  `json:"folder"`
}

func (h *RemoteBackups) putDestination(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	var body remoteDestinationRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	current, err := h.DB.RemoteDestination(r.Context())
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	d, err := h.destinationRequest(current, &body)
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	audit := &store.AuditEntry{
		UserID: u.ID,
		Action: "remote_backups.destination.update",
		Detail: detailJSON(
			map[string]any{"destination_id": d.ID, remoteKindField: d.Kind, "enabled": d.Enabled},
		),
		IP: clientIP(r.Context()),
	}
	if err := h.DB.SaveRemoteDestination(r.Context(), d, audit); err != nil {
		remoteAPIError(w, r, err)
		return
	}
	h.getDestination(w, r)
}

func (h *RemoteBackups) destinationRequest(
	current *store.RemoteDestination,
	body *remoteDestinationRequest,
) (*store.RemoteDestination, error) {
	d := &store.RemoteDestination{
		ID: store.NewID(), Kind: body.Kind, Enabled: body.Enabled,
		Endpoint: strings.TrimRight(strings.TrimSpace(body.Endpoint), "/"), Username: body.Username,
		RemoteName: body.RemoteName, Folder: strings.TrimSpace(body.Folder),
	}
	if d.Folder != "" && !remote.ValidKey(d.Folder) {
		return nil, remote.ErrConfiguration
	}
	if current != nil && current.Kind == d.Kind && current.Endpoint == d.Endpoint && current.Username == d.Username &&
		current.RemoteName == d.RemoteName && current.Folder == d.Folder {
		d.ID, d.Credentials = current.ID, current.Credentials
	}
	if err := h.destinationCredentials(d, body); err != nil {
		return nil, err
	}
	_, err := h.backend(d)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (h *RemoteBackups) remotes(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	names, err := h.rclone().Remotes(r.Context())
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, map[string]any{"items": names})
}

func (h *RemoteBackups) testDestination(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	d, err := h.DB.RemoteDestination(r.Context())
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	if d == nil {
		remoteAPIError(w, r, store.ErrRemoteUnavailable)
		return
	}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind:        jobs.KindRemoteTest,
		LockKey:     remoteBackupLock,
		RequestedBy: u.ID,
		Payload:     map[string]string{"destination_id": d.ID},
		Audit: jobAudit(
			r.Context(),
			u.ID,
			"",
			"remote_backups.destination.test",
			map[string]string{"destination_id": d.ID},
		),
	}, h.worker().Test(d))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

func remoteAPIError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrRemoteBusy),
		errors.Is(err, store.ErrRemoteUnavailable),
		errors.Is(err, store.ErrBackupProtected):
		apierr.Write(
			w,
			r,
			apierr.New(apierr.InvalidState).
				Msg("Remote storage is unavailable or busy. Check its configuration and pending uploads."),
		)
	case errors.Is(err, remote.ErrConfiguration):
		var v apierr.Validation
		v.Add(
			"destination",
			apierr.FieldInvalid,
			"Use a valid HTTPS WebDAV destination or a configured rclone remote and relative folder.",
		)
		apierr.Write(w, r, v.Err())
	default:
		var failure *remote.Failure
		if errors.As(err, &failure) {
			apierr.Write(w, r, apierr.New(apierr.Unavailable).Msg(failure.Message))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
	}
}

func (h *RemoteBackups) destinationCredentials(d *store.RemoteDestination, body *remoteDestinationRequest) error {
	switch d.Kind {
	case "webdav":
		if body.Password != nil {
			encrypted, err := h.Keeper.Encrypt(
				crypto.PurposeRemoteBackup,
				remoteCredentialsLocation(d.ID),
				[]byte(*body.Password),
			)
			if err != nil {
				return fmt.Errorf("encrypt remote credentials: %w", err)
			}
			d.Credentials = encrypted
		}
		if d.Credentials == "" || d.Username == "" || d.RemoteName != "" {
			return remote.ErrConfiguration
		}
	case "rclone":
		if d.Endpoint != "" || d.Username != "" || body.Password != nil {
			return remote.ErrConfiguration
		}
	}
	return nil
}
