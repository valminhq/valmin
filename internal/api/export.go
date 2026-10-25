package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/export"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Exports serves the installation export: POST /admin/export writes one, GET /admin/export
// describes the latest and GET /admin/export/download returns it. The archive holds the master
// key and the database, so every endpoint is panel.settings, which a member sees as absent.
type Exports struct {
	DB       *store.DB
	Authz    *authz.Authz
	Engine   *jobs.Engine
	Exporter *export.Exporter
}

func exportRoutes(rt *routeTable, h *Exports) {
	rt.Handle("POST /api/v1/admin/export", http.HandlerFunc(h.create))
	rt.Handle("GET /api/v1/admin/export", http.HandlerFunc(h.latest))
	rt.Stream("GET /api/v1/admin/export/download", http.HandlerFunc(h.download))
}

// exportRequest is the body of POST /admin/export.
type exportRequest struct {
	Mode string `json:"mode"`
}

// latestExportView is GET /admin/export: the latest export, null when there is none.
type latestExportView struct {
	Latest *exportView `json:"latest"`
}

// exportView describes one finished export.
type exportView struct {
	Filename  string    `json:"filename"`
	Mode      string    `json:"mode"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// create submits an export job. It holds the lock of every server not running, so it is
// refused while a job runs on one, and none starts until it ends. A running server stays
// unlocked, so the supervisor keeps watching it while its files are copied live.
func (h *Exports) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body exportRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	mode, err := export.ParseMode(body.Mode)
	if err != nil {
		writeFieldError(w, r, "mode", apierr.FieldInvalid, "Choose a light or a full export.")
		return
	}
	instances, err := h.DB.ListInstances(r.Context(), nil)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	locks := make([]string, 0, len(instances))
	var running []string
	for i := range instances {
		if instance.State(instances[i].State) == instance.StateRunning {
			running = append(running, instances[i].ID)
			continue
		}
		locks = append(locks, jobs.InstanceLockKey(instances[i].ID))
	}
	ip := middleware.ClientIPFrom(r.Context()).String()
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind:        jobs.KindExport,
		LockKey:     jobs.GlobalLockKey(jobs.KindExport),
		LockKeys:    locks,
		Payload:     exportRequest{Mode: string(mode)},
		RequestedBy: u.ID,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: u.ID, Action: authz.PanelSettings.String(),
				Detail: fmt.Sprintf(`{"operation":"export","mode":%q}`, mode), IP: ip,
			}); err != nil {
				return fmt.Errorf("audit export: %w", err)
			}
			return nil
		},
	}, h.Exporter.Job(mode, running))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// latest describes the newest finished export.
func (h *Exports) latest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	path, fi, err := h.findLatest()
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	var view latestExportView
	if fi != nil {
		view.Latest = &exportView{
			Filename: filepath.Base(path), Mode: string(export.ModeOf(path)),
			SizeBytes: fi.Size(), CreatedAt: fi.ModTime().UTC(),
		}
	}
	JSON(w, r, http.StatusOK, view)
}

// download streams the newest finished export.
func (h *Exports) download(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	path, fi, err := h.findLatest()
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if fi == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	f, err := os.Open(path) //nolint:gosec // found in the export directory, never named by the request
	if errors.Is(err, os.ErrNotExist) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	defer func() { _ = f.Close() }()
	name := filepath.Base(path)
	// The archive carries the master key, so who took a copy is on record.
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, Action: authz.PanelSettings.String(),
		Detail: fmt.Sprintf(`{"operation":"export_download","filename":%q}`, name),
		IP:     middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	// ServeContent answers Range requests, so an interrupted download resumes.
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// findLatest finds the newest finished export and its file info, a nil one when there is none.
func (h *Exports) findLatest() (path string, fi os.FileInfo, err error) {
	path, err = export.Latest(h.Exporter.Root)
	if err != nil || path == "" {
		return "", nil, err //nolint:wrapcheck // Latest names the listing
	}
	fi, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("inspect export: %w", err)
	}
	return path, fi, nil
}
