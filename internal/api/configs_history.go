package api

import (
	"context"
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/store"
)

// configRevisionView is one entry of a config file's history. User is set for a panel edit,
// Mod and the versions for a mod install, update or downgrade.
type configRevisionView struct {
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	Source        string    `json:"source"`
	User          *string   `json:"user"`
	Mod           *string   `json:"mod"`
	FromVersion   *string   `json:"from_version"`
	ToVersion     *string   `json:"to_version"`
	ReplacedEdits bool      `json:"replaced_edits"`
	Baseline      bool      `json:"baseline"`
	SizeBytes     int       `json:"size_bytes"`
}

type configHistoryView struct {
	Items      []configRevisionView `json:"items"`
	NextCursor *string              `json:"next_cursor"`
}

// modReplacedView names the mod version whose install last replaced a file holding edits.
type modReplacedView struct {
	Mod       string    `json:"mod"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *Instances) configHistoryRoutes(rt *routeTable) {
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/history", http.HandlerFunc(h.listConfigHistory))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/history/{rev}", http.HandlerFunc(h.readConfigRevision))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/history/{rev}/raw",
		http.HandlerFunc(h.readConfigRevisionRaw))
}

// listConfigHistory handles GET /instances/{id}/configs/{file}/history: the file's kept
// versions, newest first, without their content. The list is bounded, so it is one page.
func (h *Instances) listConfigHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	dir, name, ok := h.resolveConfig(w, r, inst)
	if !ok {
		return
	}
	_ = dir.Close()
	revs, err := h.DB.ConfigRevisions(r.Context(), inst.ID, name)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	view := configHistoryView{Items: make([]configRevisionView, 0, len(revs))}
	for i := range revs {
		view.Items = append(view.Items, toConfigRevisionView(&revs[i]))
	}
	JSON(w, r, http.StatusOK, view)
}

// readConfigRevision handles GET /instances/{id}/configs/{file}/history/{rev}: one kept version
// of a `.cfg`, projected through the same schema as the file itself.
func (h *Instances) readConfigRevision(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	dir, name, ok := h.resolveSettingsConfig(w, r, inst)
	if !ok {
		return
	}
	_ = dir.Close()
	rev, ok := h.loadConfigRevision(w, r, inst.ID, name)
	if !ok {
		return
	}
	JSON(w, r, http.StatusOK, configCopyView{
		Schema:     modconfig.Parse(rev.Content).Schema(r.PathValue("file")),
		CapturedAt: rev.CreatedAt,
	})
}

// readConfigRevisionRaw handles GET /instances/{id}/configs/{file}/history/{rev}/raw: one kept
// version's bytes, gated on ConfigRaw as the live file's are.
func (h *Instances) readConfigRevisionRaw(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRaw, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	dir, name, ok := h.resolveConfig(w, r, inst)
	if !ok {
		return
	}
	_ = dir.Close()
	rev, ok := h.loadConfigRevision(w, r, inst.ID, name)
	if !ok {
		return
	}
	w.Header().Set("ETag", listETag(rev.Content))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rev.Content)
}

// loadConfigRevision reads the revision named by {rev} of the file name, writing a 404 when the
// file has no such revision.
func (h *Instances) loadConfigRevision(
	w http.ResponseWriter, r *http.Request, instanceID, name string,
) (*store.ConfigRevision, bool) {
	rev, err := h.DB.ConfigRevision(r.Context(), instanceID, name, r.PathValue("rev"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if rev == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, false
	}
	return rev, true
}

// withModReplaced marks each listed file whose newest version is a mod install that replaced
// edited bytes.
func (h *Instances) withModReplaced(ctx context.Context, instanceID string, items []configFileView) error {
	latest, err := h.DB.LatestConfigRevisions(ctx, instanceID)
	if err != nil {
		return err
	}
	for i := range items {
		rev, ok := latest[items[i].File]
		if ok && rev.Source == store.ConfigMod && rev.ReplacedEdits {
			items[i].ModReplacedEdits = &modReplacedView{Mod: rev.Mod, Version: rev.ToVersion, CreatedAt: rev.CreatedAt}
		}
	}
	return nil
}

func toConfigRevisionView(rev *store.ConfigRevision) configRevisionView {
	return configRevisionView{
		ID: rev.ID, CreatedAt: rev.CreatedAt, Source: string(rev.Source),
		User: optional(rev.UserName), Mod: optional(rev.Mod),
		FromVersion: optional(rev.FromVersion), ToVersion: optional(rev.ToVersion),
		ReplacedEdits: rev.ReplacedEdits, Baseline: rev.Baseline, SizeBytes: rev.Size,
	}
}

// optional is s, or nil when it is empty.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
