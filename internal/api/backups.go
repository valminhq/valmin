package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/store"
)

// backupView is one catalogue row on the wire. It carries no path: 11 §8.3 keeps a raw
// filesystem path out of every response, which is why downloads address an archive by id.
type backupView struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instance_id"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	WorldName  string    `json:"world_name"`
	Trigger    string    `json:"trigger"`
	Consistent bool      `json:"consistent"`
	CreatedAt  time.Time `json:"created_at"`
	// Filename is what a download will be called, so a client need not rebuild the name.
	Filename string `json:"filename"`
	// PrunesNext is true when retention would remove this archive on its next run. The
	// daemon decides it because retention is one policy (02 §4.4 step 7); a client counting
	// rows of its own would be a second copy that drifts, and it can only see one page.
	PrunesNext bool `json:"prunes_next"`
}

func toBackupView(b *store.Backup) backupView {
	return backupView{
		ID: b.ID, InstanceID: b.InstanceID, SizeBytes: b.SizeBytes, SHA256: b.SHA256,
		WorldName: b.WorldName, Trigger: b.Trigger, Consistent: b.Consistent,
		CreatedAt: b.CreatedAt, Filename: filepath.Base(b.Path),
	}
}

// listBackups is GET /instances/{id}/backups (04 §3).
func (h *Instances) listBackups(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsList, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.loadVisible(w, r)
	if !ok {
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

	// One more than asked for, so the page knows there is a next one without a second
	// COUNT (11 §4).
	rows, err := h.DB.ListBackups(r.Context(), id, cursor.SortKey, cursor.ID, limit+1)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: store.FormatTime(last.CreatedAt), ID: last.ID}.Encode()
		next = &encoded
	}

	doomed, err := h.doomedArchives(r.Context(), inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	views := make([]backupView, 0, len(rows))
	for i := range rows {
		v := toBackupView(&rows[i])
		v.PrunesNext = doomed[v.ID]
		views = append(views, v)
	}
	JSON(w, r, http.StatusOK, NewPage(views, next))
}

// doomedArchives is the set of archive ids retention would remove on its next run. It runs
// the pruner the backup job runs, over the whole catalogue rather than the page being served,
// because retention counts each class across every archive an instance has.
func (h *Instances) doomedArchives(
	ctx context.Context, inst *store.Instance,
) (map[string]bool, error) {
	rows, err := h.DB.ListBackups(ctx, inst.ID, "", "", pruneScanLimit)
	if err != nil {
		return nil, fmt.Errorf("read the catalogue for instance %s: %w", inst.ID, err)
	}
	entries := make([]backup.Entry, 0, len(rows))
	for i := range rows {
		entries = append(entries, backup.Entry{ID: rows[i].ID, Consistent: rows[i].Consistent})
	}
	policy := backup.Policy{KeepCold: inst.BackupKeepCold, KeepHot: inst.BackupKeepHot}
	doomed := make(map[string]bool)
	for _, a := range backup.Prune(entries, policy) {
		doomed[a.ID] = true
	}
	return doomed, nil
}

// downloadBackup is GET /instances/{id}/backups/{bid}/download (04 §3, 11 §8.3). The path
// served comes from the catalogue row, never from the request (D13).
func (h *Instances) downloadBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsDownload, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	b, ok := h.mustLoadBackup(w, r, id)
	if !ok {
		return
	}

	// b.Path is the row's, written by the job that made the archive (D13).
	f, err := os.Open(b.Path)
	if errors.Is(err, os.ErrNotExist) {
		// The row outlived its file. A statement about this archive, not a panel fault, and
		// never a truncated 200.
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	defer func() { _ = f.Close() }()

	name := filepath.Base(b.Path)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", b.SizeBytes))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	// ServeContent answers Range requests, so an interrupted download of a large world
	// resumes rather than restarts.
	http.ServeContent(w, r, name, b.CreatedAt, f)
}

// deleteBackup is DELETE /instances/{id}/backups/{bid} (04 §3), gated on backups.restore:
// there is no backups.delete action (ADR-126).
func (h *Instances) deleteBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsRestore, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	b, ok := h.mustLoadBackup(w, r, id)
	if !ok {
		return
	}

	// The file first, then the row: a crash between them leaves a row naming nothing, which
	// download answers as 404. The other order leaves a file nothing names, unprunable.
	if err := os.Remove(b.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if err := h.DB.DeleteBackup(r.Context(), id, b.ID); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mustLoadBackup resolves {bid} within the instance the caller is already authorized for.
// An archive belonging to another instance is 404, the same as one that never existed
// (ADR-038).
func (h *Instances) mustLoadBackup(
	w http.ResponseWriter, r *http.Request, instanceID string,
) (*store.Backup, bool) {
	if _, ok := h.loadVisible(w, r); !ok {
		return nil, false
	}
	b, err := h.DB.BackupByID(r.Context(), instanceID, strings.TrimSpace(r.PathValue("bid")))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	if b == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, false
	}
	return b, true
}
