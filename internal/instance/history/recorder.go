// Package history persists the player observations and identities each instance's log reader
// reports, without blocking the reader.
package history

import (
	"context"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// Retention is the maximum age of player observations.
const Retention = 30 * 24 * time.Hour

const (
	pruneInterval    = time.Hour
	observationQueue = 256
)

type recorded struct {
	instanceID string
	obs        instance.PlayerObservation
	identity   *instance.PlayerIdentity
}

// Recorder persists log observations without blocking the log reader.
type Recorder struct {
	db        *store.DB
	queue     chan recorded
	now       func() time.Time
	retention time.Duration
	lastPrune time.Time
}

// New constructs a recorder with the default retention period.
func New(db *store.DB) *Recorder {
	return &Recorder{
		db: db, queue: make(chan recorded, observationQueue),
		now: time.Now, retention: Retention,
	}
}

// Observe accepts an observation without blocking the caller.
func (p *Recorder) Observe(instanceID string, obs instance.PlayerObservation) {
	select {
	case p.queue <- recorded{instanceID: instanceID, obs: obs}:
	default:
		slog.Warn("dropped a player observation; the history will be coarse", slog.String("instance_id", instanceID))
	}
}

// Identified accepts a player identity without blocking the caller.
func (p *Recorder) Identified(instanceID string, id instance.PlayerIdentity) {
	select {
	case p.queue <- recorded{instanceID: instanceID, identity: &id}:
	default:
		slog.Warn("dropped a player sighting", slog.String("instance_id", instanceID))
	}
}

// Run drains observations until ctx is cancelled.
func (p *Recorder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-p.queue:
			p.write(ctx, rec)
		}
	}
}

func (p *Recorder) write(ctx context.Context, rec recorded) {
	if rec.identity != nil {
		p.writeIdentity(ctx, rec.instanceID, *rec.identity)
		return
	}
	at := rec.obs.TS
	if at.IsZero() {
		at = p.now()
	}
	if err := p.db.RecordPlayerObservation(ctx, rec.instanceID, at, rec.obs.Players); err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(
				ctx,
				"could not record a player observation",
				slog.String("instance_id", rec.instanceID),
				slog.Any("error", err),
			)
		}
		return
	}
	p.maybePrune(ctx)
}

func (p *Recorder) writeIdentity(ctx context.Context, instanceID string, id instance.PlayerIdentity) {
	at := id.TS
	if at.IsZero() {
		at = p.now()
	}
	row := store.PlayerIdentity{PlatformID: id.PlatformID, Name: id.Name, FirstSeenAt: at, LastSeenAt: at}
	if err := p.db.RecordPlayerIdentity(ctx, instanceID, &row); err != nil && ctx.Err() == nil {
		slog.WarnContext(
			ctx,
			"could not record a player sighting",
			slog.String("instance_id", instanceID),
			slog.Any("error", err),
		)
	}
}

func (p *Recorder) maybePrune(ctx context.Context) {
	now := p.now()
	if now.Sub(p.lastPrune) < pruneInterval {
		return
	}
	p.lastPrune = now
	if _, err := p.db.PrunePlayerObservations(ctx, now.Add(-p.retention)); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "could not prune player history", slog.Any("error", err))
	}
}
