package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

const (
	syncBatchSize  = 200
	DefaultTimeout = 30 * time.Minute
)

// Syncer schedules and runs registry index refreshes.
type Syncer struct {
	DB       *store.DB
	Engine   *jobs.Engine
	Clients  map[source.Source]*thunderstore.Client
	Interval time.Duration
	Timeout  time.Duration
}

func (m *Syncer) timeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return DefaultTimeout
}

func ETagKey(s source.Source) string           { return s.String() + "_etag" }
func SyncedAtKey(s source.Source) string       { return s.String() + "_synced_at" }
func SyncResultKey(s source.Source) string     { return s.String() + "_sync_result" }
func ListingStartedKey(s source.Source) string { return s.String() + "_listing_started_at" }

// Run is the sync scheduler: a clock, not a worker (12 §11) — it only ever enqueues, on
// the interval the operator configured, and never executes the sync itself. It returns
// when ctx is cancelled.
func (m *Syncer) Run(ctx context.Context) {
	if m.Interval <= 0 {
		return
	}
	// Once at startup, before the first tick: otherwise a fresh panel has an empty catalogue for
	// a whole sync interval. Cheap to repeat, since later syncs send `If-None-Match` and a 304
	// writes nothing (ADR-015). Still a clock and never a worker (12 §11): it enqueues, and a
	// lock already held is skipped.
	m.Enqueue(ctx)

	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Enqueue(ctx)
		}
	}
}

// syncSpec is the sync job's spec — one lock key, no per-run payload. The kind's wire name
// stays thunderstore_sync although it now covers every registry: job kinds are persisted, and
// renaming one would leave every historical row naming a kind no build recognises. A function
// rather than a package var so a caller never risks sharing one *jobs.Spec across two
// submissions.
func SyncSpec() *jobs.Spec {
	return &jobs.Spec{
		Kind:    jobs.KindThunderstoreSync,
		LockKey: jobs.GlobalLockKey(jobs.KindThunderstoreSync),
		Payload: struct{}{},
	}
}

// enqueueSync submits a sync job covering every enabled registry. A lock already held is not a warning worth
// a log line — ADR-030's "the scheduler skips and records": the running sync will finish
// on its own, and the job history is the record, not this call.
func (m *Syncer) Enqueue(ctx context.Context) {
	_, err := m.Engine.Submit(ctx, SyncSpec(), m.RunSync)
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
func (m *Syncer) RunSync(ctx context.Context, h *jobs.Handle) jobs.Outcome {
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
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
		if writeErr := m.DB.KVSet(ctx, SyncResultKey(src), result); writeErr != nil {
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
func (m *Syncer) syncRegistry(
	ctx context.Context, h *jobs.Handle, src source.Source, client *thunderstore.Client,
) error {
	var etag string
	if _, err := m.DB.KVGet(ctx, ETagKey(src), &etag); err != nil {
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
		row, vs, err := ToStoreRows(&p, src)
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

	if err := m.DB.KVSet(ctx, ETagKey(src), result.ETag); err != nil {
		return fmt.Errorf("write etag: %w", err)
	}
	if err := m.DB.KVSet(ctx, SyncedAtKey(src), store.Now()); err != nil {
		return fmt.Errorf("write synced_at: %w", err)
	}
	// Last, and never on a 304 or a failed listing: only a listing that landed whole can say
	// what the registry does not carry.
	if err := m.DB.KVSet(ctx, ListingStartedKey(src), started); err != nil {
		return fmt.Errorf("write listing start: %w", err)
	}

	h.Log(fmt.Sprintf("%s: synced %d packages", src, total))
	return nil
}

func syncFailed(err error) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Unavailable.String(), Error: err.Error()}
}

// toStoreRows maps one thunderstore.Package onto its store rows. Description, latest_version,
// downloads and icon_url are derived from Latest() and TotalDownloads(), the v1 listing carrying
// none of them at the top level (F7).
func ToStoreRows(p *thunderstore.Package, src source.Source) (store.ModPackage, []store.ModVersion, error) {
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
