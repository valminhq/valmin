package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// PlayerHistoryRetention bounds how far back the observed-count history goes. Thirty days is
// the span an operator asks about ("was anyone on last weekend"); beyond it the rows answer
// nothing anyone asks and the audit trail, which is permanent, covers what was done rather
// than who was watching.
const PlayerHistoryRetention = 30 * 24 * time.Hour

// pruneInterval is how often the recorder sweeps. It is checked on a write rather than driven
// by a ticker: a panel nobody plays on has nothing to prune.
const pruneInterval = time.Hour

// observationQueue buffers the log reader's observations. The reader must never block on the
// database (C21): the writer pool is one connection, so a slow write would stall the console
// of every instance. A full queue drops the observation, which shows up as a coarser history
// rather than a wrong one.
const observationQueue = 256

type recorded struct {
	instanceID string
	obs        instance.PlayerObservation
	// identity is set instead of obs for a line that named an account. One queue for both:
	// they come from the same read loop under the same no-blocking rule, and the writer
	// connection they share is one.
	identity *instance.PlayerIdentity
}

// PlayerRecorder persists what the log readers observe. One goroutine for the whole panel:
// the writes are rare — a row per change, not per line — and serialising them keeps the single
// writer connection free for everything else.
type PlayerRecorder struct {
	db        *store.DB
	queue     chan recorded
	now       func() time.Time
	retention time.Duration
	lastPrune time.Time
}

// NewPlayerRecorder builds the recorder at the default retention.
func NewPlayerRecorder(db *store.DB) *PlayerRecorder {
	return &PlayerRecorder{
		db:        db,
		queue:     make(chan recorded, observationQueue),
		now:       time.Now,
		retention: PlayerHistoryRetention,
	}
}

// Observe is the log reader's callback, and never blocks.
func (p *PlayerRecorder) Observe(instanceID string, obs instance.PlayerObservation) {
	select {
	case p.queue <- recorded{instanceID: instanceID, obs: obs}:
	default:
		slog.Warn("dropped a player observation; the history will be coarse",
			slog.String("instance_id", instanceID))
	}
}

// Identified is the log reader's other callback, and never blocks. A dropped sighting costs
// a row the next one rewrites: the socket line repeats on every connection.
func (p *PlayerRecorder) Identified(instanceID string, id instance.PlayerIdentity) {
	select {
	case p.queue <- recorded{instanceID: instanceID, identity: &id}:
	default:
		slog.Warn("dropped a player sighting",
			slog.String("instance_id", instanceID))
	}
}

// Run drains the queue until ctx is cancelled.
func (p *PlayerRecorder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-p.queue:
			p.write(ctx, rec)
		}
	}
}

func (p *PlayerRecorder) write(ctx context.Context, rec recorded) {
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
			slog.WarnContext(ctx, "could not record a player observation",
				slog.String("instance_id", rec.instanceID), slog.Any("error", err))
		}
		return
	}
	p.maybePrune(ctx)
}

func (p *PlayerRecorder) writeIdentity(
	ctx context.Context, instanceID string, id instance.PlayerIdentity,
) {
	at := id.TS
	if at.IsZero() {
		at = p.now()
	}
	row := store.PlayerIdentity{
		PlatformID: id.PlatformID, Name: id.Name, FirstSeenAt: at, LastSeenAt: at,
	}
	if err := p.db.RecordPlayerIdentity(ctx, instanceID, &row); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "could not record a player sighting",
			slog.String("instance_id", instanceID), slog.Any("error", err))
	}
}

func (p *PlayerRecorder) maybePrune(ctx context.Context) {
	now := p.now()
	if now.Sub(p.lastPrune) < pruneInterval {
		return
	}
	p.lastPrune = now
	if _, err := p.db.PrunePlayerObservations(ctx, now.Add(-p.retention)); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "could not prune player history", slog.Any("error", err))
	}
}

// playerObservationView is one point of the history. `players` null is an observation gap and
// must render as one: a client that draws it as zero reports an empty server where the panel
// only meant it had stopped looking.
type playerObservationView struct {
	ID         string `json:"id"`
	ObservedAt string `json:"observed_at"`
	Players    *int   `json:"players"`
}

// playerHistory is GET /instances/{id}/players/history, newest first. Authorized on stats.read,
// the same action as the live count it is the history of.
func (h *Instances) playerHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.StatsRead, id) {
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

	rows, err := h.DB.ListPlayerObservations(r.Context(), inst.ID, cursor.SortKey, cursor.ID, limit+1)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		encoded := Cursor{SortKey: store.FormatTime(last.ObservedAt), ID: last.ID}.Encode()
		next = &encoded
	}

	items := make([]playerObservationView, 0, len(rows))
	for _, row := range rows {
		items = append(items, playerObservationView{
			ID:         row.ID,
			ObservedAt: store.FormatTime(row.ObservedAt),
			Players:    row.Players,
		})
	}
	JSON(w, r, http.StatusOK, NewPage(items, next))
}
