package api

import (
	"encoding/csv"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// Audit serves the permanent trail. audit.read is on the never-grantable list, so Can is
// checked with an empty instanceID — a global action — and a member sees the endpoints as
// absent.
type Audit struct {
	DB    *store.DB
	Authz *authz.Authz
}

func (a *Audit) Routes(rt *Router) {
	rt.Handle("GET /api/v1/audit", http.HandlerFunc(a.list))
	rt.Handle("GET /api/v1/audit/filters", http.HandlerFunc(a.filters))
	rt.Stream("GET /api/v1/audit/export", http.HandlerFunc(a.export))
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
	Outcome    *string `json:"outcome"`
	JobID      *string `json:"job_id"`
	JobError   *string `json:"job_error"`
	CreatedAt  string  `json:"created_at"`
}

// toAuditView renders a record. A job-backed entry reports the job's current status as its
// outcome; a job the retention sweep has already removed keeps the outcome stored with the entry.
func toAuditView(rec *store.AuditRecord) auditView {
	outcome := rec.Outcome
	if rec.JobStatus != nil {
		resolved := store.AuditRequested
		switch *rec.JobStatus {
		case "succeeded", "failed", "cancelled":
			resolved = *rec.JobStatus
		}
		outcome = &resolved
	}
	return auditView{
		ID: rec.ID, UserID: rec.UserID, Actor: rec.Actor,
		InstanceID: rec.InstanceID, Instance: rec.Instance,
		Action: rec.Action, Detail: rec.Detail, IP: rec.IP,
		Outcome: outcome, JobID: rec.JobID, JobError: rec.JobError,
		CreatedAt: store.FormatTime(rec.CreatedAt),
	}
}

// parseAuditFilter reads the closed allowlist of filters. An unrecognised parameter is a typo
// or a probe, and either way a filter that silently does nothing is the failure shape this
// project designs against. extra names the parameters the calling route adds.
func parseAuditFilter(r *http.Request, extra ...string) (store.AuditFilter, error) {
	for name := range r.URL.Query() {
		switch name {
		case "instance_id", "user_id", "action", "since", "until":
		default:
			if !slices.Contains(extra, name) {
				return store.AuditFilter{}, apierr.New(apierr.InvalidParameter).With("parameter", name)
			}
		}
	}
	q := r.URL.Query()
	filter := store.AuditFilter{
		InstanceID: q.Get("instance_id"),
		UserID:     q.Get("user_id"),
		Action:     q.Get("action"),
	}
	for name, dest := range map[string]*time.Time{"since": &filter.Since, "until": &filter.Until} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return store.AuditFilter{}, apierr.New(apierr.InvalidParameter).With("parameter", name).Wrap(err)
		}
		*dest = at
	}
	return filter, nil
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
	filter, err := parseAuditFilter(r, "limit", "cursor")
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

	// One more than asked for, so the page knows there is a next one without a COUNT.
	rows, err := a.DB.ListAuditLog(r.Context(), &filter, cursor.SortKey, cursor.ID, limit+1)
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
	for i := range rows {
		items = append(items, toAuditView(&rows[i]))
	}
	JSON(w, r, http.StatusOK, NewPage(items, next))
}

type auditSubjectView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type auditFiltersView struct {
	Actions   []string           `json:"actions"`
	Actors    []auditSubjectView `json:"actors"`
	Instances []auditSubjectView `json:"instances"`
}

func toSubjectViews(subjects []store.AuditSubject) []auditSubjectView {
	out := make([]auditSubjectView, 0, len(subjects))
	for _, s := range subjects {
		out = append(out, auditSubjectView{ID: s.ID, Name: s.Name})
	}
	return out
}

// filters is GET /audit/filters: the actions, actors and servers the trail contains, so the
// page's dropdowns do not depend on which rows happen to be loaded or which subjects still exist.
func (a *Audit) filters(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !a.Authz.Can(r.Context(), u, authz.AuditRead, "") {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	facets, err := a.DB.AuditFacets(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	actions := facets.Actions
	if actions == nil {
		actions = []string{}
	}
	JSON(w, r, http.StatusOK, auditFiltersView{
		Actions:   actions,
		Actors:    toSubjectViews(facets.Actors),
		Instances: toSubjectViews(facets.Instances),
	})
}

// exportBatch is how many rows one export query reads.
const exportBatch = 500

var auditCSVHeader = []string{
	"time", "actor", "user_id", "server", "instance_id", "action", "outcome", "job_id", "ip", "detail",
}

// export is GET /audit/export: every entry matching the filters as CSV, newest first.
func (a *Audit) export(w http.ResponseWriter, r *http.Request) {
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
	// The first page is read before any header goes out, so a failing query is still a JSON error.
	rows, err := a.DB.ListAuditLog(r.Context(), &filter, "", "", exportBatch)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-log.csv"`)
	out := csv.NewWriter(w)
	_ = out.Write(auditCSVHeader)
	for len(rows) > 0 {
		for i := range rows {
			v := toAuditView(&rows[i])
			_ = out.Write([]string{
				v.CreatedAt, csvCell(v.Actor), csvCell(v.UserID), csvCell(v.Instance), csvCell(v.InstanceID),
				v.Action, csvCell(v.Outcome), csvCell(v.JobID), csvCell(rows[i].IP), csvCell(v.Detail),
			})
		}
		last := rows[len(rows)-1]
		if len(rows) < exportBatch {
			break
		}
		if rows, err = a.DB.ListAuditLog(
			r.Context(), &filter, store.FormatTime(last.CreatedAt), last.ID, exportBatch,
		); err != nil {
			slog.ErrorContext(r.Context(), "audit export interrupted", "error", err)
			break
		}
	}
	out.Flush()
}

// csvCell renders an optional value, neutralising a leading character a spreadsheet would read
// as a formula.
func csvCell(v *string) string {
	if v == nil {
		return ""
	}
	if s := *v; s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return *v
}
