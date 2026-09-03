package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

// kv keys for the Thunderstore sync state (10 §4.2).
const (
	kvThunderstoreETag     = "thunderstore_etag"
	kvThunderstoreSyncedAt = "thunderstore_synced_at"
)

// syncBatchSize bounds how many packages accumulate before one write transaction flushes
// them — 12 §6: the transaction wraps the write, never the tens-of-megabytes fetch that
// produced it. Not a config key: nothing has asked to tune it, and 200 packages is a few
// hundred KB of rows, nowhere near where SQLITE_BUSY becomes a real risk.
const syncBatchSize = 200

// syncTimeout bounds one sync end to end. `↯` It exists because the job's lease is
// renewed by a goroutine independent of this Runner's own progress (12 §5.2) — a stalled
// or slow-loris upstream would otherwise hang forever with an actively-renewed lease,
// holding the one global thunderstore_sync lock and starving every future scheduled sync
// until the daemon restarts. Generous against the real measured size (162 MB, ~10,500
// packages, 1 Sep 2026): even a slow connection finishes in minutes, not thirty of them.
//
// A var, not a const, only so a test can shrink it rather than waiting out thirty real
// minutes to prove a stall is actually bounded.
var syncTimeout = 30 * time.Minute

// Mods serves M2's mod engine surface — sync in this file, search and detail in
// mods_search.go. Later work packages add resolve and install to the same struct.
type Mods struct {
	DB     *store.DB
	Authz  *authz.Authz
	Engine *jobs.Engine
	Client *thunderstore.Client
	// Cache is the content-addressed zip cache a mod install downloads through (03 §6.1).
	Cache *cache.Cache
	// DataRoot is 10 §1.1's data.root, for the install job's staging area.
	DataRoot string
	// SyncInterval is 10 §1.1's thunderstore.sync_interval — how often Run enqueues a
	// sync. Zero disables the ticker rather than panicking on time.NewTicker(0).
	SyncInterval time.Duration
}

func (m *Mods) Routes(rt *Router) {
	rt.Handle("GET /api/v1/mods/search", http.HandlerFunc(m.search))
	rt.Handle("GET /api/v1/mods/{namespace}/{name}", http.HandlerFunc(m.packageDetail))
	rt.Handle("POST /api/v1/instances/{id}/mods/resolve", http.HandlerFunc(m.resolve))
	rt.Handle("GET /api/v1/instances/{id}/mods", http.HandlerFunc(m.listInstalledMods))
	rt.Handle("POST /api/v1/instances/{id}/mods", http.HandlerFunc(m.installMods))
	rt.Handle("DELETE /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.uninstallMod))
	rt.Handle("PATCH /api/v1/instances/{id}/mods/{full_name}", http.HandlerFunc(m.patchMod))
	m.Engine.RegisterCancelPolicy(jobs.KindModInstall, modInstallCancelPolicy)
	m.Engine.RegisterCancelPolicy(jobs.KindModUninstall, modUninstallCancelPolicy)
}

// Run is the sync scheduler: a clock, not a worker (12 §11) — it only ever enqueues, on
// the interval the operator configured, and never executes the sync itself. It returns
// when ctx is cancelled.
func (m *Mods) Run(ctx context.Context) {
	if m.SyncInterval <= 0 {
		return
	}
	// `↯` Once at startup, before the first tick. Without this a fresh panel has an **empty
	// mod catalogue for a whole hour** — `10 §1.1`'s default interval — and the mod screen
	// correctly reports that there is nothing to browse, which reads as the feature being
	// broken. Found 3 Sep 2026, on the first panel anyone had actually used: zero
	// `mod_packages` rows and no `thunderstore_sync` job at all.
	//
	// Cheap to repeat: the second and later syncs send `If-None-Match` and a `304` writes
	// nothing (ADR-015), so a panel that restarts often re-downloads nothing. Still a clock
	// and never a worker (12 §11) — it enqueues, and a lock already held is skipped.
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

// syncSpec is thunderstore_sync's job spec — one lock key, no per-run payload. A function
// rather than a package var so a caller never risks sharing one *jobs.Spec across two
// submissions.
func syncSpec() *jobs.Spec {
	return &jobs.Spec{
		Kind:    jobs.KindThunderstoreSync,
		LockKey: jobs.GlobalLockKey(jobs.KindThunderstoreSync),
		Payload: struct{}{},
	}
}

// enqueueSync submits a thunderstore_sync job. A lock already held is not a warning worth
// a log line — ADR-030's "the scheduler skips and records": the running sync will finish
// on its own, and the job history is the record, not this call.
func (m *Mods) enqueueSync(ctx context.Context) {
	_, err := m.Engine.Submit(ctx, syncSpec(), m.syncRun)
	if err == nil {
		return
	}
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		slog.DebugContext(ctx, "thunderstore sync already running, skipped", slog.String("job_id", conflict.JobID))
		return
	}
	slog.WarnContext(ctx, "enqueue thunderstore sync", slog.Any("error", err))
}

// syncRun is the thunderstore_sync Runner (12 §6's Work phase — no transaction of its
// own): stream the community listing, batch rows into UpsertModPackages, and record the
// ETag only once every batch has landed. A crash mid-sync leaves the ETag unchanged, so
// the next tick re-downloads the full listing rather than resuming from a partial index —
// correct and simple, since the sync is idempotent by design (12 §9.4).
func (m *Mods) syncRun(ctx context.Context, h *jobs.Handle) jobs.Outcome {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	var etag string
	if _, err := m.DB.KVGet(ctx, kvThunderstoreETag, &etag); err != nil {
		return syncFailed(fmt.Errorf("read cached etag: %w", err))
	}

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

	result, err := m.Client.Sync(ctx, etag, func(p thunderstore.Package) error {
		row, vs, err := toStoreRows(&p)
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
		return syncFailed(fmt.Errorf("sync thunderstore index: %w", err))
	}
	if result.NotModified {
		h.Log("thunderstore index unchanged since the last sync (304)")
		return jobs.Outcome{Status: "succeeded"}
	}
	if err := flush(); err != nil {
		return syncFailed(fmt.Errorf("write mod index: %w", err))
	}

	if err := m.DB.KVSet(ctx, kvThunderstoreETag, result.ETag); err != nil {
		return syncFailed(fmt.Errorf("write etag: %w", err))
	}
	if err := m.DB.KVSet(ctx, kvThunderstoreSyncedAt, store.Now()); err != nil {
		return syncFailed(fmt.Errorf("write synced_at: %w", err))
	}

	h.Progress(ctx, 100, fmt.Sprintf("synced %d packages", total))
	return jobs.Outcome{Status: "succeeded"}
}

func syncFailed(err error) jobs.Outcome {
	return jobs.Outcome{Status: "failed", ErrorCode: apierr.Unavailable.String(), Error: err.Error()}
}

// toStoreRows maps one thunderstore.Package onto its store rows — F7's derivation, not a
// field copy: the v1 listing has no top-level description, latest_version, downloads or
// icon_url, so those come from Latest() and TotalDownloads() rather than a field the
// response simply does not carry.
func toStoreRows(p *thunderstore.Package) (store.ModPackage, []store.ModVersion, error) {
	categories, err := json.Marshal(p.Categories)
	if err != nil {
		return store.ModPackage{}, nil, fmt.Errorf("encode categories for %s: %w", p.FullName, err)
	}

	latest, _ := p.Latest()
	row := store.ModPackage{
		FullName: p.FullName, Namespace: p.Owner, Name: p.Name,
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
			FullName: p.FullName, Version: v.VersionNumber,
			DependenciesJSON: string(deps), DownloadURL: v.DownloadURL, FileSize: v.FileSize,
		})
	}
	return row, versions, nil
}
