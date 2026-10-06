package api

import (
	"fmt"
	"net/http"
	"os"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup/remotecopy"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/store"
)

type remoteCopyView struct {
	*store.RemoteCopy
	SourceAvailable bool `json:"source_available"`
}

func remoteCopyResponse(c *store.RemoteCopy) remoteCopyView {
	info, err := os.Stat(c.SourcePath) //nolint:gosec // Path is loaded from the internal backup catalogue.
	return remoteCopyView{c, err == nil && info.Mode().IsRegular() && info.Size() == c.SizeBytes}
}

func (h *RemoteBackups) visibleInstance(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		remoteAPIError(w, r, err)
		return false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	return true
}

func (h *RemoteBackups) loadCopy(w http.ResponseWriter, r *http.Request) *store.RemoteCopy {
	c, err := h.DB.RemoteCopyByID(r.Context(), r.PathValue("id"), r.PathValue("copy_id"))
	if err != nil {
		remoteAPIError(w, r, err)
		return nil
	}
	if c == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
	}
	return c
}

func (h *RemoteBackups) upload(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.visibleInstance(w, r, u) {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.BackupsCreate, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	b, err := h.DB.BackupByID(r.Context(), id, r.PathValue("bid"))
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	if b == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if _, err := os.Stat(b.Path); err != nil { //nolint:gosec // Path is loaded from the internal backup catalogue.
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	audit := &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.backups.remote_copy",
		Detail: detailJSON(map[string]string{"backup_id": b.ID}), IP: clientIP(r.Context()),
	}
	c, err := h.DB.QueueRemoteCopy(r.Context(), id, b.ID, audit)
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	h.acceptCopy(w, r, c)
}

func (h *RemoteBackups) acceptCopy(w http.ResponseWriter, r *http.Request, c *store.RemoteCopy) {
	w.Header().Set("Location", fmt.Sprintf("/api/v1/instances/%s/remote-copies/%s", c.InstanceID, c.ID))
	JSON(w, r, http.StatusAccepted, map[string]any{remotecopy.CopyIDField: c.ID, "status": c.Status, "job_id": c.JobID})
}

func (h *RemoteBackups) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.visibleInstance(w, r, u) {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.BackupsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	limit, err := ParseLimit(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	cursor, _, err := ParseCursor(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	rows, err := h.DB.ListRemoteCopies(r.Context(), id, cursor.SortKey, cursor.ID, limit+1)
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: last.CreatedAt, ID: last.ID}.Encode()
		next = &encoded
	}
	views := make([]remoteCopyView, 0, len(rows))
	for i := range rows {
		views = append(views, remoteCopyResponse(&rows[i]))
	}
	summary, err := h.DB.RemoteSummary(r.Context(), id)
	if err != nil {
		remoteAPIError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, map[string]any{"items": views, "next_cursor": next, "summary": summary})
}

func (h *RemoteBackups) getCopy(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.visibleInstance(w, r, u) {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsList, r.PathValue("id")) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	c := h.loadCopy(w, r)
	if c == nil {
		return
	}
	JSON(w, r, http.StatusOK, remoteCopyResponse(c))
}

func (h *RemoteBackups) retry(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.visibleInstance(w, r, u) {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsCreate, r.PathValue("id")) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	c := h.loadCopy(w, r)
	if c == nil {
		return
	}
	if !remoteCopyResponse(c).SourceAvailable {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).Msg("The local archive is no longer available."))
		return
	}
	audit := &store.AuditEntry{
		UserID: u.ID, InstanceID: c.InstanceID, Action: "instances.backups.remote_retry",
		Detail: detailJSON(map[string]string{remotecopy.CopyIDField: c.ID}), IP: clientIP(r.Context()),
	}
	if err := h.DB.RetryRemoteCopy(r.Context(), c.InstanceID, c.ID, audit); err != nil {
		remoteAPIError(w, r, err)
		return
	}
	c = h.loadCopy(w, r)
	if c == nil {
		return
	}
	h.acceptCopy(w, r, c)
}

func (h *RemoteBackups) cancel(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.visibleInstance(w, r, u) {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsCreate, r.PathValue("id")) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	c := h.loadCopy(w, r)
	if c == nil {
		return
	}
	audit := &store.AuditEntry{
		UserID: u.ID, InstanceID: c.InstanceID, Action: "instances.backups.remote_cancel",
		Detail: detailJSON(map[string]string{remotecopy.CopyIDField: c.ID}), IP: clientIP(r.Context()),
	}
	if err := h.DB.CancelRemoteCopy(r.Context(), c.InstanceID, c.ID, audit); err != nil {
		remoteAPIError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
