package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance/history"
	"github.com/valminhq/valmin/internal/store"
)

// playerObservationView is one point of the history. `players` null is an observation gap and
// must render as one: a client that draws it as zero reports an empty server where the panel
// only meant it had stopped looking.
type playerObservationView struct {
	ID         string `json:"id"`
	ObservedAt string `json:"observed_at"`
	Players    *int   `json:"players"`
}

type playerHistoryRange struct {
	From    string                 `json:"from"`
	To      string                 `json:"to"`
	Initial *playerObservationView `json:"initial"`
}

type rangedPlayerHistoryPage struct {
	Page[playerObservationView]
	Range playerHistoryRange `json:"range"`
}

func playerObservationResponse(row store.PlayerObservation) playerObservationView {
	return playerObservationView{
		ID: row.ID, ObservedAt: store.FormatTime(row.ObservedAt), Players: row.Players,
	}
}

func playerHistoryBounds(r *http.Request, now time.Time) (from, to time.Time, ranged bool, err error) {
	query := r.URL.Query()
	fromText, toText := query.Get("from"), query.Get("to")
	if fromText == "" && toText == "" {
		return time.Time{}, time.Time{}, false, nil
	}
	invalid := func(name string) error {
		return apierr.New(apierr.InvalidParameter).With("parameter", name).
			Wrap(fmt.Errorf("invalid player history range"))
	}
	if fromText == "" {
		return time.Time{}, time.Time{}, false, invalid("from")
	}
	if toText == "" {
		return time.Time{}, time.Time{}, false, invalid("to")
	}
	from, err = time.Parse(time.RFC3339Nano, fromText)
	if err != nil {
		return time.Time{}, time.Time{}, false, invalid("from")
	}
	to, err = time.Parse(time.RFC3339Nano, toText)
	if err != nil {
		return time.Time{}, time.Time{}, false, invalid("to")
	}
	if to.After(now) {
		to = now
	}
	if !from.Before(to) || to.Sub(from) > history.Retention {
		return time.Time{}, time.Time{}, false, invalid("from")
	}
	return from, to, true, nil
}

func playerHistoryInitial(
	ctx context.Context, db *store.DB, instanceID string, from time.Time,
) (*playerObservationView, error) {
	initial, err := db.PlayerObservationBefore(ctx, instanceID, from)
	if err != nil {
		return nil, fmt.Errorf("find starting player count: %w", err)
	}
	if initial == nil {
		return nil, nil
	}
	view := playerObservationResponse(*initial)
	return &view, nil
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
	from, to, ranged, err := playerHistoryBounds(r, time.Now())
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	var rows []store.PlayerObservation
	if ranged {
		rows, err = h.DB.ListPlayerObservationsInRange(
			r.Context(), inst.ID, from, to, cursor.SortKey, cursor.ID, limit+1)
	} else {
		rows, err = h.DB.ListPlayerObservations(r.Context(), inst.ID, cursor.SortKey, cursor.ID, limit+1)
	}
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
		items = append(items, playerObservationResponse(row))
	}
	page := NewPage(items, next)
	if ranged {
		initial, err := playerHistoryInitial(r.Context(), h.DB, inst.ID, from)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		JSON(w, r, http.StatusOK, rangedPlayerHistoryPage{
			Page:  page,
			Range: playerHistoryRange{From: store.FormatTime(from), To: store.FormatTime(to), Initial: initial},
		})
		return
	}
	JSON(w, r, http.StatusOK, page)
}
