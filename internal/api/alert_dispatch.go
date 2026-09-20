package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// paramsWire is the JSON form of a rule's thresholds. Durations are seconds with the unit in
// the field name (11 §1); the evaluator's own struct keeps them typed.
type paramsWire struct {
	CrashCount         int     `json:"crash_count,omitempty"`
	CrashWindowSeconds int     `json:"crash_window_seconds,omitempty"`
	StuckAfterSeconds  int     `json:"stuck_after_seconds,omitempty"`
	StaleFactor        float64 `json:"stale_factor,omitempty"`
}

// decodeParams reads a rule's stored thresholds. Unreadable JSON falls back to the defaults
// rather than failing the scan: one bad rule must not stop every condition being evaluated.
func decodeParams(raw string) alerts.Params {
	var w paramsWire
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return alerts.Params{}
		}
	}
	return alerts.Params{
		CrashCount:  w.CrashCount,
		CrashWindow: time.Duration(w.CrashWindowSeconds) * time.Second,
		StuckAfter:  time.Duration(w.StuckAfterSeconds) * time.Second,
		StaleFactor: w.StaleFactor,
	}
}

// paramsWireOf reads a rule's stored thresholds back into their wire form.
func paramsWireOf(raw string) paramsWire {
	var w paramsWire
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return paramsWire{}
		}
	}
	return w
}

func encodeParams(w paramsWire) (string, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encode alert thresholds: %w", err)
	}
	return string(raw), nil
}

// dispatchAlerts sends one message per rule per edge. A failure is logged and nothing else: a
// notification never changes the outcome it reports.
func (h *Instances) dispatchAlerts(ctx context.Context, diff store.ConditionDiff) {
	if h.Notify == nil {
		return
	}
	rules, err := h.DB.ListAlertRules(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read alert rules", slog.Any("error", err))
		return
	}
	names := h.instanceNames(ctx)
	now := time.Now().UTC()

	for _, edge := range []struct {
		name       string
		kind       notify.Kind
		conditions []store.AlertCondition
	}{
		{store.EdgeOpened, notify.KindAlertOpened, diff.Opened},
		{store.EdgeResolved, notify.KindAlertResolved, diff.Resolved},
	} {
		for i := range edge.conditions {
			h.dispatchOne(ctx, &edge.conditions[i], edge.name, edge.kind, rules, names, now)
		}
	}
}

func (h *Instances) dispatchOne(
	ctx context.Context, c *store.AlertCondition, edge string, kind notify.Kind,
	rules []store.AlertRule, names map[string]string, now time.Time,
) {
	for i := range rules {
		r := &rules[i]
		if !matches(r, c) {
			continue
		}
		// A rule inside its quiet window is left unclaimed, so the next scan after the window
		// ends sends it -- provided the condition is still true.
		if quiet(r, now) {
			continue
		}
		claimed, err := h.DB.MarkNotified(ctx, c.ID, r.ID, edge, now)
		if err != nil {
			slog.ErrorContext(ctx, "claim alert notification",
				slog.String("condition_id", c.ID), slog.Any("error", err))
			continue
		}
		if !claimed {
			continue
		}
		h.Notify.EmitTo(ctx, alertEvent(c, kind, edge, names), r.WebhookIDs)
	}
}

// matches reports whether a rule covers this condition. A rule with no instance covers every
// one, including a host-level condition.
func matches(r *store.AlertRule, c *store.AlertCondition) bool {
	if !r.Enabled || r.ConditionKind != c.Kind {
		return false
	}
	if r.InstanceID == nil {
		return true
	}
	return c.InstanceID != nil && *c.InstanceID == *r.InstanceID
}

// quiet reports whether now falls inside a rule's quiet window, in the rule's own timezone. A
// window whose start is above its end wraps past midnight.
func quiet(r *store.AlertRule, now time.Time) bool {
	if r.QuietStart == nil || r.QuietEnd == nil || r.QuietTZ == nil {
		return false
	}
	loc, err := time.LoadLocation(*r.QuietTZ)
	if err != nil {
		return false
	}
	local := now.In(loc)
	minutes := local.Hour()*60 + local.Minute()
	start, end := *r.QuietStart, *r.QuietEnd
	if start <= end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

func alertEvent(
	c *store.AlertCondition, kind notify.Kind, edge string, names map[string]string,
) *notify.Event {
	detail := map[string]string{"Condition": c.Kind}
	for k, v := range c.Detail {
		detail[k] = v
	}
	e := &notify.Event{
		ID:         store.NewID(),
		Kind:       kind,
		OccurredAt: time.Now().UTC(),
		Summary:    summarize(c.Kind, edge),
		Detail:     detail,
	}
	if c.InstanceID != nil {
		e.InstanceID = *c.InstanceID
		e.InstanceName = names[*c.InstanceID]
	}
	return e
}

// conditionSentence is what each condition says when it opens. The panel writes the sentence;
// the wire kind stays generic.
var conditionSentence = map[string]string{
	alerts.KindJobFailed.String():       "A job failed",
	alerts.KindLowDisk.String():         "The host is running out of disk space",
	alerts.KindStaleBackup.String():     "Scheduled backups are not running",
	alerts.KindUncleanStop.String():     "A server stopped before its world finished saving",
	alerts.KindRestartRequired.String(): "A server needs restarting to apply a change",
	alerts.KindUpdateAvailable.String(): "A server update is available",
	alerts.KindInstanceError.String():   "A server is in an error state",
	alerts.KindCrashLoop.String():       "A server is crashing repeatedly",
	alerts.KindJobStuck.String():        "A job has been running unusually long",
}

func summarize(kind, edge string) string {
	sentence, ok := conditionSentence[kind]
	if !ok {
		sentence = kind
	}
	if edge == store.EdgeResolved {
		return "Cleared: " + sentence
	}
	return sentence
}

// instanceNames maps instance ids to names for the notification body. A read failure costs the
// names, not the notification.
func (h *Instances) instanceNames(ctx context.Context) map[string]string {
	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		slog.WarnContext(ctx, "read instance names", slog.Any("error", err))
		return nil
	}
	out := make(map[string]string, len(instances))
	for i := range instances {
		out[instances[i].ID] = instances[i].Name
	}
	return out
}
