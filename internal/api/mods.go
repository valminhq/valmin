package api

import (
	"context"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

// Mods serves the mod engine surface: sync in this file, search and detail in
// mods_search.go, resolve and install alongside them.
type Mods struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	// Clients holds one client per enabled registry (03 §6.1). A registry the operator
	// disabled is absent rather than flagged, so nothing downstream checks twice.
	Clients map[source.Source]*thunderstore.Client
	// Profiles stores profile codes for each enabled registry that offers them.
	Profiles profileHosts
	// Patterns reads the loader log's plugin lines.
	Patterns instance.PatternSet
	plan     *manager.Planner
	install  *manager.Installer
	syncer   *manager.Syncer
}

// enabledSources keeps resolution and search in the registry preference order.
func (m *Mods) enabledSources() []source.Source {
	out := make([]source.Source, 0, len(m.Clients))
	for _, src := range source.All() {
		if _, ok := m.Clients[src]; ok {
			out = append(out, src)
		}
	}
	return out
}

// indexedPackage reads one package's index row, preferring the named registry and falling
// back to the others in the order source.All fixes. A nil row is a package no registry has,
// which callers report as not found rather than as an error.
//
// allowed narrows which registries may answer; nil admits every one of them, including a
// registry the operator has disabled. That distinction is the point: the installed list and
// the uninstall preview describe packages whose files are already on disk, so they keep
// their names and deprecation flags when a registry is switched off, while the catalogue
// endpoints pass the enabled set and stop offering what no install would accept.
//
// It is the one place the preference rule lives, so those callers cannot drift about which
// registry's description they show.
func (m *Mods) indexedPackage(
	ctx context.Context, fullName string, prefer source.Source, allowed []source.Source,
) (*store.ModPackage, error) {
	//nolint:wrapcheck // preserve the catalogue read error
	return manager.IndexedPackage(
		ctx,
		m.DB,
		fullName,
		prefer,
		allowed,
	)
}

func modRoutes(rt *routeTable, m *Mods) {
	rt.Handle("POST /api/v1/mods/sync", http.HandlerFunc(m.sync))
	rt.Handle("GET /api/v1/mods/search", http.HandlerFunc(m.search))
	rt.Handle("GET /api/v1/mods/{namespace}/{name}", http.HandlerFunc(m.packageDetail))
	rt.Handle("POST /api/v1/instances/{id}/mods/resolve", http.HandlerFunc(m.resolve))
	rt.Handle("GET /api/v1/instances/{id}/mods", http.HandlerFunc(m.listInstalledMods))
	rt.Handle("POST /api/v1/instances/{id}/mods", http.HandlerFunc(m.installMods))
	rt.Handle("DELETE /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.uninstallMod))
	rt.Handle("PATCH /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.patchMod))
	rt.Stream("GET /api/v1/instances/{id}/mods/{full_name}/changelog", http.HandlerFunc(m.changelog))
	rt.Handle("GET /api/v1/instances/{id}/mods/export", http.HandlerFunc(m.exportClientManifest))
	rt.Handle("POST /api/v1/instances/{id}/mods/export/code", http.HandlerFunc(m.exportClientCode))
	rt.Handle("POST /api/v1/instances/{id}/mods/code/preview", http.HandlerFunc(m.previewCode))
	rt.Handle("POST /api/v1/instances/{id}/mods/code/apply", http.HandlerFunc(m.applyCode))
	rt.Handle("POST /api/v1/instances/{id}/mods/updates/resolve", http.HandlerFunc(m.previewUpdates))
	rt.Handle("POST /api/v1/instances/{id}/mods/updates", http.HandlerFunc(m.applyUpdates))
	rt.Handle("POST /api/v1/instances/{id}/mods/updates/queue", http.HandlerFunc(m.queueModUpdates))
	rt.Handle("GET /api/v1/instances/{id}/mods/queue", http.HandlerFunc(m.listModQueue))
	rt.Handle("POST /api/v1/instances/{id}/mods/queue", http.HandlerFunc(m.queueModInstall))
	rt.Handle("DELETE /api/v1/instances/{id}/mods/queue/{full_name}", http.HandlerFunc(m.unqueueModInstall))
}

// sync is POST /mods/sync: refresh every enabled registry now rather than at the next tick.
// A member sees the endpoint as absent, and a sync already running answers 409 with its job.
func (m *Mods) sync(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	spec := manager.SyncSpec()
	spec.RequestedBy = u.ID
	job, err := m.Engine.Submit(r.Context(), spec, m.syncer.RunSync)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// Run schedules registry refreshes until ctx is cancelled.
func (m *Mods) Run(ctx context.Context) { m.syncer.Run(ctx) }
