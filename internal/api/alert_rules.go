package api

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/store"
)

// AlertRules serves /admin/alert-rules. Admin-only on panel.settings, the same gate the
// destinations it routes to already use.
type AlertRules struct {
	DB    *store.DB
	Authz *authz.Authz
}

func alertRuleRoutes(rt *routeTable, h *AlertRules) {
	rt.Handle("GET /api/v1/admin/alert-rules", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/admin/alert-rules", http.HandlerFunc(h.create))
	rt.Handle("PATCH /api/v1/admin/alert-rules/{id}", http.HandlerFunc(h.patch))
	rt.Handle("DELETE /api/v1/admin/alert-rules/{id}", http.HandlerFunc(h.delete))
}

type alertRuleView struct {
	ID            string            `json:"id"`
	InstanceID    *string           `json:"instance_id"`
	ConditionKind string            `json:"condition_kind"`
	Params        alerts.ParamsWire `json:"params"`
	QuietStart    *int              `json:"quiet_start_minutes"`
	QuietEnd      *int              `json:"quiet_end_minutes"`
	QuietTZ       *string           `json:"quiet_timezone"`
	Enabled       bool              `json:"enabled"`
	WebhookIDs    []string          `json:"webhook_ids"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

type alertRuleRequest struct {
	InstanceID    *string            `json:"instance_id"`
	ConditionKind *string            `json:"condition_kind"`
	Params        *alerts.ParamsWire `json:"params"`
	QuietStart    *int               `json:"quiet_start_minutes"`
	QuietEnd      *int               `json:"quiet_end_minutes"`
	QuietTZ       *string            `json:"quiet_timezone"`
	Enabled       *bool              `json:"enabled"`
	WebhookIDs    []string           `json:"webhook_ids"`
}

func toAlertRuleView(r *store.AlertRule) alertRuleView {
	ids := r.WebhookIDs
	if ids == nil {
		ids = []string{}
	}
	return alertRuleView{
		ID: r.ID, InstanceID: r.InstanceID, ConditionKind: r.ConditionKind,
		Params: alerts.ParamsWireOf(r.Params), QuietStart: r.QuietStart, QuietEnd: r.QuietEnd,
		QuietTZ: r.QuietTZ, Enabled: r.Enabled, WebhookIDs: ids,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (h *AlertRules) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	rows, err := h.DB.ListAlertRules(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	views := make([]alertRuleView, 0, len(rows))
	for i := range rows {
		views = append(views, toAlertRuleView(&rows[i]))
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

func (h *AlertRules) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body alertRuleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	rule := &store.AlertRule{ID: store.NewID(), Enabled: true, CreatedBy: &u.ID, Params: "{}"}
	if !h.apply(w, r, rule, &body) {
		return
	}
	if err := h.DB.SaveAlertRule(r.Context(), rule); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "alert_rule_create", rule.ID)
	JSON(w, r, http.StatusCreated, toAlertRuleView(rule))
}

func (h *AlertRules) patch(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	rule, err := h.DB.AlertRuleByID(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if rule == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body alertRuleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !h.apply(w, r, rule, &body) {
		return
	}
	if err := h.DB.SaveAlertRule(r.Context(), rule); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "alert_rule_update", rule.ID)
	JSON(w, r, http.StatusOK, toAlertRuleView(rule))
}

func (h *AlertRules) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	rule, err := h.DB.AlertRuleByID(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if rule == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if err := h.DB.DeleteAlertRule(r.Context(), rule.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, u.ID, "alert_rule_delete", rule.ID)
	w.WriteHeader(http.StatusNoContent)
}

// audit records who changed a rule. The change has already committed, so a failure is
// logged rather than returned.
func (h *AlertRules) audit(r *http.Request, userID, operation, ruleID string) {
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: userID, Action: authz.PanelSettings.String(),
		Detail: fmt.Sprintf(`{"operation":%q,"alert_rule_id":%q}`, operation, ruleID),
		IP:     middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		slog.ErrorContext(r.Context(), "audit alert rule change",
			slog.String("operation", operation), slog.Any("error", err))
	}
}

// apply validates a request onto a rule, answering 422 itself when it cannot. A field the
// request omits is left as it was, so a rename cannot clear the destinations.
func (h *AlertRules) apply(
	w http.ResponseWriter, r *http.Request, rule *store.AlertRule, body *alertRuleRequest,
) bool {
	var v apierr.Validation
	if body.ConditionKind != nil {
		if _, ok := alerts.ParseKind(*body.ConditionKind); !ok {
			v.Add("condition_kind", apierr.FieldNotAnOption, "Not a condition this panel evaluates.")
		} else {
			rule.ConditionKind = *body.ConditionKind
		}
	}
	if rule.ConditionKind == "" {
		v.Add("condition_kind", apierr.FieldRequired, "A rule must name a condition.")
	}
	if body.InstanceID != nil {
		h.checkInstance(r, &v, *body.InstanceID)
		rule.InstanceID = body.InstanceID
		if *body.InstanceID == "" {
			rule.InstanceID = nil
		}
	}
	// Host-level conditions carry no instance, so a server-scoped rule for one never matches.
	scoping := body.ConditionKind != nil || body.InstanceID != nil
	if kind, _ := alerts.ParseKind(rule.ConditionKind); scoping && alerts.HostWide(kind) && rule.InstanceID != nil {
		v.Add("instance_id", apierr.FieldNotAnOption, "This alert is host-wide and cannot name a server.")
	}
	if body.Params != nil {
		checkParams(&v, body.Params)
		checkMessageOptions(&v, rule.ConditionKind, body.Params)
		raw, err := alerts.EncodeParams(*body.Params)
		if err != nil {
			v.Add("params", apierr.FieldNotAnOption, "Thresholds could not be stored.")
		} else {
			rule.Params = raw
		}
	}
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.WebhookIDs != nil {
		h.checkDestinations(r, &v, body.WebhookIDs)
		rule.WebhookIDs = body.WebhookIDs
	}
	applyQuietHours(&v, rule, body)

	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return false
	}
	return true
}

// checkParams range-checks thresholds. Zero is the default for every field, as alerts.Params
// reads it.
func checkParams(v *apierr.Validation, p *alerts.ParamsWire) {
	for field, value := range map[string]int{
		"crash_count": p.CrashCount, "crash_window_seconds": p.CrashWindowSeconds,
		"stuck_after_seconds": p.StuckAfterSeconds,
	} {
		if value < 0 {
			v.Add("params."+field, apierr.FieldOutOfRange, "Zero for the default, or above it.")
		}
	}
	// A duration past this overflows time.Duration and would be stored meaning something else.
	for field, value := range map[string]int{
		"crash_window_seconds": p.CrashWindowSeconds, "stuck_after_seconds": p.StuckAfterSeconds,
	} {
		if int64(value) > math.MaxInt64/int64(time.Second) {
			v.Add("params."+field, apierr.FieldOutOfRange, "Too long to hold as a duration.")
		}
	}
	if p.StaleFactor != 0 && p.StaleFactor <= 1 {
		v.Add("params.stale_factor", apierr.FieldOutOfRange, "Zero for the default, or above 1.")
	}
}

// checkMessageOptions checks the fields a rule hides and the zone its messages show times in.
func checkMessageOptions(v *apierr.Validation, conditionKind string, p *alerts.ParamsWire) {
	kind, _ := alerts.ParseKind(conditionKind)
	for _, f := range p.HiddenFields {
		if !slices.Contains(alerts.MessageFields(kind), f) {
			v.Add("params.hidden_fields", apierr.FieldNotAnOption, "Not a field this alert sends: "+f+".")
			break
		}
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			v.Add("params.timezone", apierr.FieldNotAnOption, "Not a timezone this host knows.")
		}
	}
}

func (h *AlertRules) checkInstance(r *http.Request, v *apierr.Validation, id string) {
	if id == "" {
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil || inst == nil {
		v.Add("instance_id", apierr.FieldNotAnOption, "No such instance.")
	}
}

func (h *AlertRules) checkDestinations(r *http.Request, v *apierr.Validation, ids []string) {
	known, err := h.DB.ListWebhooks(r.Context())
	if err != nil {
		v.Add("webhook_ids", apierr.FieldNotAnOption, "Destinations could not be read.")
		return
	}
	exists := make(map[string]bool, len(known))
	for i := range known {
		exists[known[i].ID] = true
	}
	for _, id := range ids {
		if !exists[id] {
			v.Add("webhook_ids", apierr.FieldNotAnOption, "No such destination: "+id+".")
			return
		}
	}
}

// applyQuietHours sets or clears the window. The three fields move together: a window with no
// timezone is a window whose meaning depends on where the daemon happens to run.
func applyQuietHours(v *apierr.Validation, rule *store.AlertRule, body *alertRuleRequest) {
	if body.QuietStart == nil && body.QuietEnd == nil && body.QuietTZ == nil {
		return
	}
	if body.QuietStart == nil || body.QuietEnd == nil || body.QuietTZ == nil {
		v.Add("quiet_timezone", apierr.FieldRequired,
			"Quiet hours need a start, an end and a timezone together.")
		return
	}
	if *body.QuietTZ == "" {
		rule.QuietStart, rule.QuietEnd, rule.QuietTZ = nil, nil, nil
		return
	}
	for field, value := range map[string]int{
		"quiet_start_minutes": *body.QuietStart, "quiet_end_minutes": *body.QuietEnd,
	} {
		if value < 0 || value > 1439 {
			v.Add(field, apierr.FieldOutOfRange, "Minutes from midnight, 0 to 1439.")
		}
	}
	if _, err := time.LoadLocation(*body.QuietTZ); err != nil {
		v.Add("quiet_timezone", apierr.FieldNotAnOption, "Not a timezone this host knows.")
		return
	}
	rule.QuietStart, rule.QuietEnd, rule.QuietTZ = body.QuietStart, body.QuietEnd, body.QuietTZ
}
