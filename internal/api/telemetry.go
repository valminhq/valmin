package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// The fetch half of 14 §7.2's subscribe-then-fetch, for a console, a graph and the operational
// history behind them: what happened before anyone subscribed.

// defaultLogTail is 04 §3's own number. maxLogTail is a clamp rather than a rejection
// (11 §4's rule for limits), and sits above the ring buffer's 1000 lines so this endpoint
// can always answer at least as much as the socket would replay.
const (
	defaultLogTail = 500
	maxLogTail     = 2000
)

// logLine is one line as this endpoint reports it. There is no `seq`: those are minted by the
// panel for its ring buffer (14 §4.2) and these lines come from Docker, so the two cannot be
// spliced together.
type logLine struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// logs is GET /instances/{id}/logs?tail=500. It reads Docker rather than the ring buffer, which
// 14 §8 empties on a daemon restart: a server that died before the panel last restarted has no
// in-memory console left.
func (h *Instances) logs(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	// Two checks, two codes: an instance the caller cannot see is 404, since a 403 is an existence
	// oracle (D2, ADR-038); one they can see without console.read is 403.
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConsoleRead, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.loadVisible(w, r)
	if !ok {
		return
	}

	tail := defaultLogTail
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.InvalidParameter).With("parameter", "tail"))
			return
		}
		tail = min(max(n, 1), maxLogTail)
	}

	// An instance that was never provisioned has no container and therefore no log. That is
	// an empty answer, not an error: the caller asked what the server said, and it has not
	// said anything.
	if inst.ContainerID == nil {
		JSON(w, r, http.StatusOK, NewPage([]logLine{}, nil))
		return
	}

	lines, err := readContainerLog(r.Context(), h.Runtime, *inst.ContainerID, tail)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(lines, nil))
}

// readContainerLog demuxes and reassembles the container's log the same way the live reader
// does — the framing is Docker's, not the panel's, and a line can straddle a frame (E5).
func readContainerLog(
	ctx context.Context, rt runtime.Runtime, containerID string, tail int,
) (lines []logLine, err error) {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Tail: tail, Timestamps: true})
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller names the operation
	}
	defer func() { _ = rc.Close() }()

	lines = []logLine{}
	if err := instance.DemuxLines(rc, func(l instance.Line) {
		lines = append(lines, logLine{TS: l.TS, Stream: l.Stream, Line: l.Text})
	}); err != nil {
		return nil, err //nolint:wrapcheck // the caller names the operation
	}
	return lines, nil
}

// statsView is one resource reading. Every number is nullable, and that is deliberate: a
// stopped server has no resource usage, and reporting zeros for it is the same lie E10
// forbids on the first CPU sample.
type statsView struct {
	// Available is false when nothing is sampling — a stopped container, or one the panel
	// has not opened a sampler for yet.
	Available bool       `json:"available"`
	TS        *time.Time `json:"ts"`
	CPUPct    *float64   `json:"cpu_pct"`
	MemBytes  *uint64    `json:"mem_bytes"`
	MemLimit  *uint64    `json:"mem_limit"`
	MemPct    *float64   `json:"mem_pct"`
	// Players is always null (E7, Q7), on this route as on the socket.
	Players *int `json:"players"`
}

// stats is GET /instances/{id}/stats, the one-shot read behind subscribe-then-fetch for a graph
// (14 §7.2). It serves the sampler's most recent sample rather than taking its own reading: the
// CPU percentage is a delta between two samples, so a fresh read could only answer null (E10).
func (h *Instances) stats(w http.ResponseWriter, r *http.Request) {
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

	sampler := h.Streams.Sampler(inst.ID)
	if sampler == nil {
		JSON(w, r, http.StatusOK, statsView{})
		return
	}
	latest, ok := sampler.Latest()
	if !ok {
		JSON(w, r, http.StatusOK, statsView{})
		return
	}

	JSON(w, r, http.StatusOK, statsView{
		Available: true,
		TS:        &latest.TS,
		CPUPct:    latest.CPUPct,
		MemBytes:  &latest.MemBytes,
		MemLimit:  &latest.MemLimit,
		MemPct:    latest.MemPct,
		Players:   latest.Players,
	})
}

// loadVisible returns the row for an instance-scoped read. The `Can` calls are deliberately not
// in here: every handler makes them at its own call site, because a check hidden in a helper
// fails open unreported (ADR-037).
func (h *Instances) loadVisible(w http.ResponseWriter, r *http.Request) (*store.Instance, bool) {
	return h.mustLoadInstance(w, r, strings.TrimSpace(r.PathValue("id")))
}

// jobHistory is GET /instances/{id}/jobs, this instance's job rows, newest first (ADR-099).
// Two things the detail page must show live only here: `running (registration unconfirmed)`
// (ADR-043) and `clean=false` after a stop where the save line was never seen (12 §3.4).
// Without this route the SPA could learn them only by having watched the job happen.
//
// Authorized on instance.view alone, matching GET /jobs/{id}: the same rows by another index.
func (h *Instances) jobHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if _, ok := h.loadVisible(w, r); !ok {
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

	// One more than asked for: the extra row is how the page knows there is a next one
	// without a second COUNT (11 §4 — next_cursor null is the end, and there is no has_more
	// to disagree with it).
	rows, err := h.DB.ListJobsForInstance(r.Context(), id, cursor.SortKey, cursor.ID, limit+1)
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

	views := make([]jobView, 0, len(rows))
	for i := range rows {
		views = append(views, toJobView(&rows[i]))
	}
	JSON(w, r, http.StatusOK, NewPage(views, next))
}

// diskView is one instance's footprint. Every figure is allocated bytes — what `du` reports
// — because that is what an operator will check it against (instance.DiskUsage).
type diskView struct {
	TotalBytes   uint64    `json:"total_bytes"`
	ServerBytes  uint64    `json:"server_bytes"`
	WorldsBytes  uint64    `json:"worlds_bytes"`
	LogsBytes    uint64    `json:"logs_bytes"`
	BackupsBytes uint64    `json:"backups_bytes"`
	MeasuredAt   time.Time `json:"measured_at"`
}

// disk is GET /instances/{id}/disk.
//
// Its own route rather than more fields on /stats, which serves an in-memory sample in
// microseconds while this walks the instance's directory tree (measured at 12 ms for a SteamCMD
// install): too slow behind a two-second graph, and still worth reading on a stopped instance,
// where /stats reports `available: false`. Uncached, so it cannot be wrong right after the
// delete an operator is watching for.
//
// Authorized identically to /stats: instance.view for existence, stats.read for the numbers.
func (h *Instances) disk(w http.ResponseWriter, r *http.Request) {
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

	// A filesystem walk, and therefore never inside a transaction (C1, C2). It is not in
	// one here; the note is for whoever later decides to cache the result in a table.
	usage, err := instance.DiskUsage(
		inst.DataDir, filepath.Join(instance.BackupsDir(h.Cfg.Data.Root), inst.ID))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	JSON(w, r, http.StatusOK, diskView{
		TotalBytes:   usage.Total,
		ServerBytes:  usage.Server,
		WorldsBytes:  usage.Worlds,
		LogsBytes:    usage.Logs,
		BackupsBytes: usage.Backups,
		MeasuredAt:   time.Now().UTC(),
	})
}
