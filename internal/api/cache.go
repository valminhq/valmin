package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Caches serves the cache inventory and its prune: GET /admin/cache lists every cached mod
// archive, game build and setup file with who uses it, and POST /admin/cache/prune removes what
// nothing uses. Both are panel.settings, which a member sees as absent.
type Caches struct {
	Authz  *authz.Authz
	Pruner *control.CachePruner
}

// cacheRoutes registers the cache inventory and prune routes.
func cacheRoutes(rt *routeTable, h *Caches) {
	rt.Handle("GET /api/v1/admin/cache", http.HandlerFunc(h.inventory))
	rt.Handle("POST /api/v1/admin/cache/prune", http.HandlerFunc(h.prune))
}

// cacheInventoryView is GET /admin/cache: every entry in one page, totals per kind, and the
// grace period after which an unused entry may be pruned.
type cacheInventoryView struct {
	Page[cacheEntryView]
	Summary      []cacheSummaryView `json:"summary"`
	GraceSeconds int64              `json:"grace_seconds"`
}

// cacheSummaryView totals one kind of entry.
type cacheSummaryView struct {
	Kind             string `json:"kind"`
	Count            int    `json:"count"`
	SizeBytes        int64  `json:"size_bytes"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
}

// cacheEntryView is one cache entry. Registry is set for mod archives and their partial
// downloads; prunable_at is null while a server or saved setup uses the entry, and prunable
// reports that a prune now removes it.
type cacheEntryView struct {
	Kind             string            `json:"kind"`
	Name             string            `json:"name"`
	Registry         *string           `json:"registry"`
	SizeBytes        int64             `json:"size_bytes"`
	ReclaimableBytes int64             `json:"reclaimable_bytes"`
	InUse            bool              `json:"in_use"`
	Prunable         bool              `json:"prunable"`
	Servers          []cacheServerView `json:"servers"`
	Setups           int               `json:"setups"`
	PrunableAt       *time.Time        `json:"prunable_at"`
}

// cacheServerView names a server using an entry.
type cacheServerView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cacheKinds orders the summary.
var cacheKinds = []control.CacheKind{
	control.CacheModArchive, control.CacheGameBuild, control.CacheSetupBlob, control.CachePartialDownload,
}

// inventory lists the caches against what servers and saved setups use.
func (h *Caches) inventory(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	entries, err := h.Pruner.Inventory(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toCacheInventoryView(entries))
}

// toCacheInventoryView renders entries and totals them per kind.
func toCacheInventoryView(entries []control.CacheEntry) cacheInventoryView {
	totals := make(map[control.CacheKind]*cacheSummaryView, len(cacheKinds))
	view := cacheInventoryView{
		Summary:      make([]cacheSummaryView, len(cacheKinds)),
		GraceSeconds: int64(control.CacheGrace / time.Second),
	}
	for i, k := range cacheKinds {
		view.Summary[i].Kind = string(k)
		totals[k] = &view.Summary[i]
	}
	items := make([]cacheEntryView, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		item := cacheEntryView{
			Kind: string(e.Kind), Name: e.Name, SizeBytes: e.SizeBytes,
			ReclaimableBytes: e.ReclaimableBytes, InUse: e.InUse(), Prunable: e.Prunable, Setups: e.Setups,
			Servers: make([]cacheServerView, 0, len(e.Servers)),
		}
		if e.Registry != "" {
			item.Registry = &e.Registry
		}
		if !e.PrunableAt.IsZero() {
			at := e.PrunableAt.UTC()
			item.PrunableAt = &at
		}
		for _, s := range e.Servers {
			item.Servers = append(item.Servers, cacheServerView{ID: s.ID, Name: s.Name})
		}
		items = append(items, item)
		if t := totals[e.Kind]; t != nil {
			t.Count++
			t.SizeBytes += e.SizeBytes
			t.ReclaimableBytes += e.ReclaimableBytes
		}
	}
	view.Page = NewPage(items, nil)
	return view
}

// prune submits a cache prune. It is refused with job_in_progress while any job or sequence
// runs on a server, a setup is being saved or restored, or an export runs.
func (h *Caches) prune(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	ip := middleware.ClientIPFrom(r.Context()).String()
	job, err := h.Pruner.Submit(r.Context(), &jobs.Spec{
		RequestedBy: u.ID,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: u.ID, Action: authz.PanelSettings.String(),
				Detail: `{"operation":"cache_prune"}`, IP: ip,
			}); err != nil {
				return fmt.Errorf("audit cache prune: %w", err)
			}
			return nil
		},
	})
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
