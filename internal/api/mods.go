package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

// kv keys for one registry's sync state (10 §4.2). The names are derived from the registry's
// own, so Thunderstore's keys are the ones it has always used and a second registry needs no
// migration to get its own.
func kvETag(s source.Source) string       { return s.String() + "_etag" }
func kvSyncedAt(s source.Source) string   { return s.String() + "_synced_at" }
func kvSyncResult(s source.Source) string { return s.String() + "_sync_result" }

// kvListingStarted is when the registry's last complete listing began. Every row that listing
// carried is stamped at or after it, so a row stamped before it is one the listing left out.
func kvListingStarted(s source.Source) string { return s.String() + "_listing_started_at" }

// syncBatchSize bounds how many packages accumulate before one write transaction flushes them.
// The transaction wraps the write, never the fetch that produced it (12 §6).
const syncBatchSize = 200

// syncTimeout bounds one sync end to end. The lease is renewed independently of this Runner's
// progress (12 §5.2), so without it a stalled upstream would hold the global sync lock forever.
// Generous against the measured listing size, where even a slow connection finishes in minutes.
// A var so a test can shrink it.
var syncTimeout = 30 * time.Minute

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
	Caches map[source.Source]*cache.Cache
	// DataRoot is 10 §1.1's data.root, for the install job's staging area.
	DataRoot string
	// ArchiveWorlds takes the world archive "Update all" promises before it changes a file. It is
	// the instance handlers' own snapshot, handed in because they are built first; the returned
	// callback records the archive in the job's Finish transaction.
	ArchiveWorlds func(inst *store.Instance, trigger string) (func(context.Context, *sql.Tx) error, error)
	// SyncInterval is 10 §1.1's thunderstore.sync_interval — how often Run enqueues a
	// sync. Zero disables the ticker rather than panicking on time.NewTicker(0).
	SyncInterval time.Duration
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
	rows, err := m.DB.ModPackagesByFullName(ctx, fullName)
	if err != nil {
		return nil, fmt.Errorf("read the index row for %s: %w", fullName, err)
	}
	// Narrowed before the preference below runs, never after: a package both registries
	// carry would otherwise resolve to a disabled one whenever source.All happens to reach
	// it first, and report the package missing although an enabled registry has it.
	if allowed != nil {
		rows = slices.DeleteFunc(rows, func(row store.ModPackage) bool {
			return !slices.Contains(allowed, row.Source)
		})
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// The named registry first, then the others in the order source.All fixes, so a package
	// both carry resolves the same way on every page rather than by how the names sort.
	for _, want := range append([]source.Source{prefer}, source.All()...) {
		for i := range rows {
			if rows[i].Source == want {
				return &rows[i], nil
			}
		}
	}
	return &rows[0], nil
}

func (m *Mods) Routes(rt *Router) {
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
	m.Engine.RegisterCancelPolicy(jobs.KindModInstall, modInstallCancelPolicy)
	m.Engine.RegisterCancelPolicy(jobs.KindModUninstall, modUninstallCancelPolicy)
	m.Engine.RegisterCancelPolicy(jobs.KindModToggle, modToggleCancelPolicy)
}

// Run is the sync scheduler: a clock, not a worker (12 §11) — it only ever enqueues, on
// the interval the operator configured, and never executes the sync itself. It returns
// when ctx is cancelled.
func (m *Mods) Run(ctx context.Context) {
	if m.SyncInterval <= 0 {
		return
	}
	// Once at startup, before the first tick: otherwise a fresh panel has an empty catalogue for
	// a whole sync interval. Cheap to repeat, since later syncs send `If-None-Match` and a 304
	// writes nothing (ADR-015). Still a clock and never a worker (12 §11): it enqueues, and a
	// lock already held is skipped.
	m.enqueueSync(ctx)

	ticker := time.NewTicker(m.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.enqueueSync(ctx)
		}
	}
}

// syncSpec is the sync job's spec — one lock key, no per-run payload. The kind's wire name
// stays thunderstore_sync although it now covers every registry: job kinds are persisted, and
// renaming one would leave every historical row naming a kind no build recognises. A function
// rather than a package var so a caller never risks sharing one *jobs.Spec across two
// submissions.
func syncSpec() *jobs.Spec {
	return &jobs.Spec{
		Kind:    jobs.KindThunderstoreSync,
		LockKey: jobs.GlobalLockKey(jobs.KindThunderstoreSync),
		Payload: struct{}{},
	}
}

// enqueueSync submits a sync job covering every enabled registry. A lock already held is not a warning worth
// a log line — ADR-030's "the scheduler skips and records": the running sync will finish
// on its own, and the job history is the record, not this call.
func (m *Mods) enqueueSync(ctx context.Context) {
	_, err := m.Engine.Submit(ctx, syncSpec(), m.syncRun)
	if err == nil {
		return
	}
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		slog.DebugContext(ctx, "mod index sync already running, skipped",
			slog.String("job_id", conflict.JobID))
		return
	}
	slog.WarnContext(ctx, "enqueue mod index sync", slog.Any("error", err))
}

// syncRun is the sync Runner, holding no transaction of its own (12 §6): stream each enabled
// registry's listing, batch its rows into UpsertModPackages, and record that registry's ETag
// only once its batches have landed. A crash leaves the ETag unchanged, so the next tick
// re-downloads the full listing, the sync being idempotent (12 §9.4).
//
// One registry failing never stops another's rows from landing: the job fails only when every
// enabled registry failed, because a panel with one stale catalogue is more useful than a
// panel with none.
func (m *Mods) syncRun(ctx context.Context, h *jobs.Handle) jobs.Outcome {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	var failures []error
	synced := 0
	for _, src := range source.All() {
		client, ok := m.Clients[src]
		if !ok {
			continue
		}
		synced++
		err := m.syncRegistry(ctx, h, src, client)
		result := diag.RegistrySync{CheckedAt: time.Now().UTC(), OK: err == nil}
		if err != nil {
			result.Error = err.Error()
		}
		if writeErr := m.DB.KVSet(ctx, kvSyncResult(src), result); writeErr != nil {
			slog.WarnContext(ctx, "record registry refresh result", slog.Any("error", writeErr))
		}
		if err != nil {
			slog.ErrorContext(ctx, "sync mod registry",
				slog.String("source", src.String()), slog.Any("error", err))
			h.Log(fmt.Sprintf("%s: %v", src, err))
			failures = append(failures, fmt.Errorf("%s: %w", src, err))
		}
	}

	if synced > 0 && len(failures) == synced {
		return syncFailed(errors.Join(failures...))
	}
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// syncRegistry streams one registry's listing into the index. A registry that sends no cache
// validators answers 200 every time and leaves the stored ETag empty, which is that host's
// normal and not a fault (03 §6.1).
func (m *Mods) syncRegistry(
	ctx context.Context, h *jobs.Handle, src source.Source, client *thunderstore.Client,
) error {
	var etag string
	if _, err := m.DB.KVGet(ctx, kvETag(src), &etag); err != nil {
		return fmt.Errorf("read cached etag: %w", err)
	}
	// Taken before the first batch is stamped, so it precedes every row this listing writes.
	started := store.Now()

	var packages []store.ModPackage
	var versions []store.ModVersion
	total := 0

	flush := func() error {
		if len(packages) == 0 {
			return nil
		}
		if err := m.DB.UpsertModPackages(ctx, packages, versions); err != nil {
			return fmt.Errorf("upsert mod batch: %w", err)
		}
		total += len(packages)
		packages, versions = packages[:0], versions[:0]
		return nil
	}

	result, err := client.Sync(ctx, etag, func(p thunderstore.Package) error {
		row, vs, err := toStoreRows(&p, src)
		if err != nil {
			return err
		}
		packages = append(packages, row)
		versions = append(versions, vs...)
		if len(packages) >= syncBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sync index: %w", err)
	}
	if result.NotModified {
		h.Log(fmt.Sprintf("%s: index unchanged since the last sync (304)", src))
		return nil
	}
	if err := flush(); err != nil {
		return fmt.Errorf("write mod index: %w", err)
	}

	if err := m.DB.KVSet(ctx, kvETag(src), result.ETag); err != nil {
		return fmt.Errorf("write etag: %w", err)
	}
	if err := m.DB.KVSet(ctx, kvSyncedAt(src), store.Now()); err != nil {
		return fmt.Errorf("write synced_at: %w", err)
	}
	// Last, and never on a 304 or a failed listing: only a listing that landed whole can say
	// what the registry does not carry.
	if err := m.DB.KVSet(ctx, kvListingStarted(src), started); err != nil {
		return fmt.Errorf("write listing start: %w", err)
	}

	h.Log(fmt.Sprintf("%s: synced %d packages", src, total))
	return nil
}

func syncFailed(err error) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: apierr.Unavailable.String(), Error: err.Error()}
}

// toStoreRows maps one thunderstore.Package onto its store rows. Description, latest_version,
// downloads and icon_url are derived from Latest() and TotalDownloads(), the v1 listing carrying
// none of them at the top level (F7).
func toStoreRows(p *thunderstore.Package, src source.Source) (store.ModPackage, []store.ModVersion, error) {
	categories, err := json.Marshal(p.Categories)
	if err != nil {
		return store.ModPackage{}, nil, fmt.Errorf("encode categories for %s: %w", p.FullName, err)
	}

	latest, _ := p.Latest()
	row := store.ModPackage{
		FullName: p.FullName, Source: src, Namespace: p.Owner, Name: p.Name,
		Description: latest.Description, LatestVersion: latest.VersionNumber,
		Downloads: p.TotalDownloads(), Rating: p.RatingScore, IsDeprecated: p.IsDeprecated,
		CategoriesJSON: string(categories), IconURL: latest.Icon,
	}

	versions := make([]store.ModVersion, 0, len(p.Versions))
	for _, v := range p.Versions {
		deps, err := json.Marshal(v.Dependencies)
		if err != nil {
			return store.ModPackage{}, nil,
				fmt.Errorf("encode dependencies for %s-%s: %w", p.FullName, v.VersionNumber, err)
		}
		versions = append(versions, store.ModVersion{
			FullName: p.FullName, Source: src, Version: v.VersionNumber,
			DependenciesJSON: string(deps), DownloadURL: v.DownloadURL, FileSize: v.FileSize,
		})
	}
	return row, versions, nil
}
