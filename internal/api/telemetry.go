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

// The reads that answer "what happened here before I was looking". A live stream says
// nothing about what came before someone subscribed, which is why 14 §7.2 makes
// subscribe-then-fetch the rule; these are the fetch half for a console, a graph and the
// operational history behind them.

// defaultLogTail is 04 §3's own number. maxLogTail is a clamp rather than a rejection
// (11 §4's rule for limits), and sits above the ring buffer's 1000 lines so this endpoint
// can always answer at least as much as the socket would replay.
const (
	defaultLogTail = 500
	maxLogTail     = 2000
)

// logLine is one line as this endpoint reports it. There is no `seq`: sequence numbers
// are the ring buffer's, minted by the panel (14 §4.2), and these lines come from Docker.
// Inventing one here would let a client believe it could splice this response into a live
// console, which is exactly what it must not do.
type logLine struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// logs is GET /instances/{id}/logs?tail=500.
//
// It reads Docker, not the ring buffer, and that is the whole point of it existing.
// 14 §8 empties the buffer on a daemon restart, so the console of a server that died last
// night has no in-memory source at all once the panel has restarted since — and that is the
// question this page is opened to answer.
func (h *Instances) logs(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	// Two checks, two codes. An instance the caller cannot see is 404 — a 403 there is an
	// existence oracle (D2, ADR-038). One they can see but hold no console.read on is 403,
	// because pretending it does not exist would be a lie they can disprove from their own
	// dashboard.
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

// stats is GET /instances/{id}/stats: the one-shot read behind subscribe-then-fetch for a
// graph (14 §7.2).
//
// It serves the sampler's most recent sample rather than taking its own reading. The CPU
// percentage is a delta between two samples (E10) — a fresh raw read has no predecessor and
// could only answer null, so a caller opening a page on a server that has been up for hours
// would be told the panel does not know what it has been sampling for hours.
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

// loadVisible authorizes the two checks every instance-scoped read makes and returns the
// row.
//
// The `Can` calls are deliberately not in here. Every handler calls it at its own call
// site (ADR-037), so the four lines are repeated in each handler and this only does the
// load. That is the cost the rule knowingly accepts, because a route-pattern or
// helper-hidden check fails open and nothing reports it.
func (h *Instances) loadVisible(w http.ResponseWriter, r *http.Request) (*store.Instance, bool) {
	return h.mustLoadInstance(w, r, strings.TrimSpace(r.PathValue("id")))
}

// jobHistory is GET /instances/{id}/jobs — this instance's job rows, newest first.
//
// Additive to 04 §3, which lists no job-history route, and recorded as ADR-099 rather
// than added quietly. Two things the detail page must show live only on a job row and
// nowhere else: ADR-043's `running (registration unconfirmed)` warning, and 12 §3.4's
// `clean=false` after a stop where the save line was never seen. Both are facts about the
// instance an operator has to be told, and without this route the SPA can only learn them
// by having watched the job happen — which is exactly the assumption 14 §7.2 forbids.
//
// Authorized on instance.view alone, matching GET /jobs/{id}: this is the same rows by a
// different index, and a viewer who may see the state may see how it got there.
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
// Its own route rather than three more fields on /stats: /stats serves the sampler's last
// in-memory sample and returns in microseconds, while this walks the instance's directory
// tree, measured at 12 ms for the 4 000 files of a SteamCMD install — fast enough to serve
// on demand, far too slow behind a graph that polls every two seconds. They also disagree
// about a stopped instance: /stats reports `available: false` because nothing is sampling,
// while disk usage is most worth reading exactly then. No cache: 12 ms does not need one,
// and a cached figure can be wrong right after the delete an operator is watching for.
//
// Authorized identically to /stats — instance.view for existence, stats.read for the
// numbers.
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
