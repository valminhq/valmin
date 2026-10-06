package api

import (
	"context"
	"net/http"
	"time"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

// kv keys for one registry's sync state (10 §4.2). The names are derived from the registry's
// own, so Thunderstore's keys are the ones it has always used and a second registry needs no
// migration to get its own.
func kvETag(s source.Source) string       { return manager.ETagKey(s) }
func kvSyncedAt(s source.Source) string   { return manager.SyncedAtKey(s) }
func kvSyncResult(s source.Source) string { return manager.SyncResultKey(s) }

// kvListingStarted is when the registry's last complete listing began. Every row that listing
// carried is stamped at or after it, so a row stamped before it is one the listing left out.
func kvListingStarted(s source.Source) string { return manager.ListingStartedKey(s) }

// Mods serves the mod engine surface: sync in this file, search and detail in
// mods_search.go, resolve and install alongside them.
type Mods struct {
	DB       *store.DB
	Authz    *authz.Authz
	Engine   *jobs.Engine
	Commands *command.Manager
	// Clients holds one client per enabled registry (03 §6.1). A registry the operator
	// disabled is absent rather than flagged, so nothing downstream checks twice.
	Clients map[source.Source]*thunderstore.Client
	// Caches is the content-addressed zip cache per registry (03 §6.1). Two registries can
	// serve different bytes under one package-version, so they never share a cache root (B14).
	Caches  map[source.Source]*cache.Cache
	plan    *manager.Planner
	install *manager.Installer
	// DataRoot is 10 §1.1's data.root, for the install job's staging area.
	DataRoot string
	// SyncInterval is 10 §1.1's thunderstore.sync_interval — how often Run enqueues a
	// sync. Zero disables the ticker rather than panicking on time.NewTicker(0).
	SyncInterval time.Duration
	SyncTimeout  time.Duration
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

func (m *Mods) planner() *manager.Planner {
	if m.plan != nil {
		return m.plan
	}
	return &manager.Planner{DB: m.DB, Enabled: m.enabledSources()}
}

func (m *Mods) installer() *manager.Installer {
	if m.install != nil {
		return m.install
	}
	return &manager.Installer{
		DB: m.DB, Engine: m.Engine, Commands: m.Commands,
		Clients: m.Clients, Caches: m.Caches, DataRoot: m.DataRoot,
	}
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
	rt.Handle("GET /api/v1/mods/search", http.HandlerFunc(m.search))
	rt.Handle("GET /api/v1/mods/{namespace}/{name}", http.HandlerFunc(m.packageDetail))
	rt.Handle("POST /api/v1/instances/{id}/mods/resolve", http.HandlerFunc(m.resolve))
	rt.Handle("GET /api/v1/instances/{id}/mods", http.HandlerFunc(m.listInstalledMods))
	rt.Handle("POST /api/v1/instances/{id}/mods", http.HandlerFunc(m.installMods))
	rt.Handle("DELETE /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.uninstallMod))
	rt.Handle("PATCH /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.patchMod))
	rt.Handle("GET /api/v1/instances/{id}/mods/export", http.HandlerFunc(m.exportClientManifest))
	rt.Handle("POST /api/v1/instances/{id}/mods/updates/resolve", http.HandlerFunc(m.previewUpdates))
	rt.Handle("POST /api/v1/instances/{id}/mods/updates", http.HandlerFunc(m.applyUpdates))
}

func (m *Mods) syncer() *manager.Syncer {
	return &manager.Syncer{
		DB: m.DB, Engine: m.Engine, Clients: m.Clients,
		Interval: m.SyncInterval, Timeout: m.SyncTimeout,
	}
}

// Run schedules registry refreshes until ctx is cancelled.
func (m *Mods) Run(ctx context.Context) { m.syncer().Run(ctx) }
