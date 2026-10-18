package api

import (
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/alerts"
	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/notify"
	deliveryjob "github.com/valminhq/valmin/internal/notify/delivery"
	"github.com/valminhq/valmin/internal/ratelimit"
	"github.com/valminhq/valmin/internal/store"
)

// AlertRules serves the alert rule endpoints in the two scopes Webhooks does: every rule on the
// admin routes, and on a server's routes the rules about that server, beside the panel-wide
// ones it can read but not change.
type AlertRules struct {
	DB         *store.DB
	Authz      *authz.Authz
	Dispatcher *deliveryjob.Dispatcher
	// TestSends is the per-user test message limit, shared with the webhook tests.
	TestSends *ratelimit.Limiter
}

func alertRuleRoutes(rt *routeTable, h *AlertRules) {
	for _, base := range []string{"/api/v1/admin/alert-rules", "/api/v1/instances/{id}/alert-rules"} {
		rt.Handle("GET "+base, http.HandlerFunc(h.list))
		rt.Handle("POST "+base, http.HandlerFunc(h.create))
		rt.Handle("PATCH "+base+"/{rid}", http.HandlerFunc(h.patch))
		rt.Handle("DELETE "+base+"/{rid}", http.HandlerFunc(h.delete))
		rt.Handle("POST "+base+"/test", http.HandlerFunc(h.test))
	}
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

// list answers the rules the scope sees.
func (h *AlertRules) list(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	rows, err := h.DB.ListAlertRules(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	writeSeen(w, r, s, rows, func(row *store.AlertRule) *string { return row.InstanceID }, toAlertRuleView)
}

// create adds a rule, about the server on a server's routes.
func (h *AlertRules) create(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	var body alertRuleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	rule := &store.AlertRule{
		ID: store.NewID(), InstanceID: s.owner(), Enabled: true, CreatedBy: &s.user.ID, Params: "{}",
	}
	if !h.apply(w, r, s, rule, &body) {
		return
	}
	if err := h.DB.SaveAlertRule(r.Context(), rule); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, s, "alert_rule_create", rule.ID)
	JSON(w, r, http.StatusCreated, toAlertRuleView(rule))
}

func (h *AlertRules) patch(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	rule, ok := h.lookup(w, r, s)
	if !ok {
		return
	}
	var body alertRuleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !h.apply(w, r, s, rule, &body) {
		return
	}
	if err := h.DB.SaveAlertRule(r.Context(), rule); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, s, "alert_rule_update", rule.ID)
	JSON(w, r, http.StatusOK, toAlertRuleView(rule))
}

func (h *AlertRules) delete(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	rule, ok := h.lookup(w, r, s)
	if !ok {
		return
	}
	if err := h.DB.DeleteAlertRule(r.Context(), rule.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.audit(r, s, "alert_rule_delete", rule.ID)
	w.WriteHeader(http.StatusNoContent)
}

// ruleTestRequest is a rule as the editor holds it, the webhook to try it on, and the sample
// values its preview fills the template with.
type ruleTestRequest struct {
	ConditionKind string            `json:"condition_kind"`
	Params        alerts.ParamsWire `json:"params"`
	WebhookID     string            `json:"webhook_id"`
	Fields        map[string]string `json:"fields"`
}

// maxTestFields bounds the sample values a rule test sends.
const maxTestFields = 16

var fieldKey = regexp.MustCompile(`^[a-z_]{1,40}$`)

// test sends the message a rule would, over sample values, to one webhook the rule may use in
// the scope. It goes out as a test notification, so no receiver takes it for the real event,
// and nothing but its delivery is stored.
func (h *AlertRules) test(w http.ResponseWriter, r *http.Request) {
	s, ok := notifyCaller(w, r, h.Authz, h.DB)
	if !ok || !s.admit(w, r, h.Authz.Can(r.Context(), s.user, s.action(), s.instanceID)) {
		return
	}
	var body ruleTestRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var v apierr.Validation
	kind, known := alerts.ParseKind(body.ConditionKind)
	if !known {
		v.Add("condition_kind", apierr.FieldNotAnOption, "Not a condition this panel evaluates.")
	}
	checkMessageOptions(&v, body.ConditionKind, &body.Params)
	checkTestFields(&v, body.Fields)
	hook, err := h.DB.WebhookByID(r.Context(), body.WebhookID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if hook == nil || !hook.Serves(s.instanceID) {
		v.Add("webhook_id", apierr.FieldNotAnOption, "No such destination for this rule.")
	}
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !allowTestSend(w, r, h.TestSends, s.user) {
		return
	}

	event := &notify.Event{
		ID: store.NewID(), Kind: notify.KindTest, OccurredAt: time.Now().UTC(),
		InstanceID: s.instanceID, InstanceName: s.instanceName,
	}
	for _, key := range slices.Sorted(maps.Keys(body.Fields)) {
		event.Detail = append(event.Detail, notify.Field{Key: key, Name: key, Value: body.Fields[key]})
	}
	deliveryjob.Compose(event, kind, &body.Params)
	job, err := h.Dispatcher.Dispatch(r.Context(), hook, event, s.user.ID)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	s.audit(r, fmt.Sprintf(`{"operation":"alert_rule_test","webhook_id":%q}`, hook.ID))
	Accepted(w, r, job.ID, toJobView(job))
}

// checkTestFields bounds a rule test's sample values to what an event's fields could hold.
func checkTestFields(v *apierr.Validation, fields map[string]string) {
	if len(fields) > maxTestFields {
		v.Add("fields", apierr.FieldOutOfRange, fmt.Sprintf("At most %d fields.", maxTestFields))
		return
	}
	for key, value := range fields {
		if !fieldKey.MatchString(key) || utf8.RuneCountInString(value) > notify.MaxDetailValue {
			v.Add("fields", apierr.FieldInvalid, "Not a field a message can name: "+key+".")
			return
		}
	}
}

// lookup reads the rule a route names, answering 404 when there is none the scope may change.
func (h *AlertRules) lookup(w http.ResponseWriter, r *http.Request, s notifyScope) (*store.AlertRule, bool) {
	rule, err := h.DB.AlertRuleByID(r.Context(), r.PathValue("rid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if rule == nil || !s.owns(rule.InstanceID) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, false
	}
	return rule, true
}

// audit records who changed a rule.
func (h *AlertRules) audit(r *http.Request, s notifyScope, operation, ruleID string) {
	s.audit(r, fmt.Sprintf(`{"operation":%q,"alert_rule_id":%q}`, operation, ruleID))
}

// apply validates a request onto a rule, answering 422 itself when it cannot. A field the
// request omits is left as it was, so a rename cannot clear the destinations.
func (h *AlertRules) apply(
	w http.ResponseWriter, r *http.Request, s notifyScope, rule *store.AlertRule, body *alertRuleRequest,
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
		h.applyInstance(r, s, &v, rule, *body.InstanceID)
	}
	// Host-level conditions carry no instance, so a server-scoped rule for one never matches.
	scoping := body.ConditionKind != nil || body.InstanceID != nil
	if kind, _ := alerts.ParseKind(rule.ConditionKind); scoping && alerts.HostWide(kind) && rule.InstanceID != nil {
		v.Add("instance_id", apierr.FieldNotAnOption, "This alert is host-wide and cannot name a server.")
	}
	if body.Params != nil {
		applyParams(&v, rule, body.Params)
	}
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.WebhookIDs != nil {
		rule.WebhookIDs = body.WebhookIDs
	}
	if body.WebhookIDs != nil || body.InstanceID != nil {
		h.checkDestinations(r, &v, rule)
	}
	applyQuietHours(&v, rule, body)

	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return false
	}
	return true
}

// applyInstance sets the server a rule is about, "" for every server, which must exist and be
// one the scope may make rules about.
func (h *AlertRules) applyInstance(
	r *http.Request, s notifyScope, v *apierr.Validation, rule *store.AlertRule, id string,
) {
	h.checkInstance(r, v, id)
	rule.InstanceID = &id
	if id == "" {
		rule.InstanceID = nil
	}
	if !s.owns(rule.InstanceID) {
		v.Add("instance_id", apierr.FieldNotAnOption, "A rule here can only be about this server.")
	}
}

// applyParams checks a rule's thresholds and message options and stores them on the rule.
func applyParams(v *apierr.Validation, rule *store.AlertRule, p *alerts.ParamsWire) {
	checkParams(v, p)
	checkMessageOptions(v, rule.ConditionKind, p)
	raw, err := alerts.EncodeParams(p)
	if err != nil {
		v.Add("params", apierr.FieldNotAnOption, "Thresholds could not be stored.")
		return
	}
	rule.Params = raw
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
	if utf8.RuneCountInString(p.Template) > maxTemplate {
		v.Add("params.template", apierr.FieldOutOfRange,
			fmt.Sprintf("At most %d characters.", maxTemplate))
	}
	if !slices.Contains(notify.Layouts, p.Layout) {
		v.Add("params.layout", apierr.FieldNotAnOption, "Not a message layout.")
	}
	if p.Color != "" && !hexColor.MatchString(p.Color) {
		v.Add("params.color", apierr.FieldInvalid, "A colour as #RRGGBB.")
	}
}

// maxTemplate bounds a rule's template, in characters.
const maxTemplate = 1000

var hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func (h *AlertRules) checkInstance(r *http.Request, v *apierr.Validation, id string) {
	if id == "" {
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil || inst == nil {
		v.Add("instance_id", apierr.FieldNotAnOption, "No such instance.")
	}
}

// checkDestinations requires every webhook a rule names to serve the rule's server: a shared
// webhook, or that server's own. Another server's webhook reads as one that does not exist.
func (h *AlertRules) checkDestinations(r *http.Request, v *apierr.Validation, rule *store.AlertRule) {
	known, err := h.DB.ListWebhooks(r.Context())
	if err != nil {
		v.Add("webhook_ids", apierr.FieldNotAnOption, "Destinations could not be read.")
		return
	}
	serves := make(map[string]bool, len(known))
	for i := range known {
		serves[known[i].ID] = known[i].Serves(deref(rule.InstanceID))
	}
	for _, id := range rule.WebhookIDs {
		if !serves[id] {
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
