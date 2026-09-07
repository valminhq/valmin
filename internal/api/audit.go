package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// Audit serves the permanent trail of 12 §7. audit.read is on 09 §3.3's never-grantable
// list, so Can is checked with an empty instanceID — a global action — and a member sees
// the endpoint as absent (ADR-038).
type Audit struct {
	DB    *store.DB
	Authz *authz.Authz
}

func (a *Audit) Routes(rt *Router) {
	rt.Handle("GET /api/v1/audit", http.HandlerFunc(a.list))
}

type auditView struct {
	ID         string  `json:"id"`
	UserID     *string `json:"user_id"`
	Actor      *string `json:"actor"`
	InstanceID *string `json:"instance_id"`
	Instance   *string `json:"instance"`
	Action     string  `json:"action"`
	Detail     *string `json:"detail"`
	IP         *string `json:"ip"`
	CreatedAt  string  `json:"created_at"`
}

// parseAuditFilter reads 04 §3's closed allowlist. An unrecognised parameter is a typo or a
// probe, and either way a filter that silently does nothing is the failure shape this
// project designs against.
func parseAuditFilter(r *http.Request) (store.AuditFilter, error) {
	for name := range r.URL.Query() {
		switch name {
		case "limit", "cursor", "instance_id", "user_id", "action":
		default:
			return store.AuditFilter{}, apierr.New(apierr.InvalidParameter).With("parameter", name)
		}
	}
	q := r.URL.Query()
	return store.AuditFilter{
		InstanceID: q.Get("instance_id"),
		UserID:     q.Get("user_id"),
		Action:     q.Get("action"),
	}, nil
}

func (a *Audit) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !a.Authz.Can(r.Context(), u, authz.AuditRead, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	filter, err := parseAuditFilter(r)
	if err != nil {
		apierr.Write(w, r, err)
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

	// One more than asked for, so the page knows there is a next one without a COUNT (11 §4).
	rows, err := a.DB.ListAuditLog(r.Context(), filter, cursor.SortKey, cursor.ID, limit+1)
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

	items := make([]auditView, 0, len(rows))
	for _, rec := range rows {
		items = append(items, auditView{
			ID: rec.ID, UserID: rec.UserID, Actor: rec.Actor,
			InstanceID: rec.InstanceID, Instance: rec.Instance,
			Action: rec.Action, Detail: rec.Detail, IP: rec.IP,
			CreatedAt: store.FormatTime(rec.CreatedAt),
		})
	}
	JSON(w, r, http.StatusOK, NewPage(items, next))
}
