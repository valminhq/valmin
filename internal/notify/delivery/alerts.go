package delivery

import (
	"context"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// clearanceHorizon is how long after a condition resolves its resolution may still be sent. A
// quiet window is shorter than a day, so this outlasts any window that held one back.
const clearanceHorizon = 48 * time.Hour

// DispatchAlerts sends one message per rule per edge still owed: the resolution of every recent
// condition whose opening a rule announced, and the opening of every open condition. It reads
// what is owed from the stored conditions rather than one scan's diff, so an edge a quiet window
// held back goes out on the first scan after the window ends. A resolution whose kind and
// instance has opened again stays held until that condition resolves too: each delivery is its
// own job, so sending both at once could land the clearing after the new alert. A failure is
// logged and nothing else: a notification never changes the outcome it reports.
func (n *Notifier) DispatchAlerts(ctx context.Context) {
	rules, err := n.DB.ListAlertRules(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read alert rules", slog.Any("error", err))
		return
	}
	if len(rules) == 0 {
		return
	}
	now := time.Now().UTC()
	open, err := n.DB.OpenConditions(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read open conditions", slog.Any("error", err))
		return
	}
	resolved, err := n.DB.ResolvedUnannounced(ctx, now.Add(-clearanceHorizon))
	if err != nil {
		slog.ErrorContext(ctx, "read unannounced resolutions", slog.Any("error", err))
		return
	}
	reopened := make(map[[2]string]bool, len(open))
	for i := range open {
		reopened[[2]string{open[i].Kind, deref(open[i].InstanceID)}] = true
	}
	owed := resolved[:0]
	for i := range resolved {
		if !reopened[[2]string{resolved[i].Kind, deref(resolved[i].InstanceID)}] {
			owed = append(owed, resolved[i])
		}
	}
	names := n.instanceNames(ctx)

	for _, edge := range []struct {
		name       string
		kind       notify.Kind
		conditions []store.AlertCondition
	}{
		{store.EdgeResolved, notify.KindAlertResolved, owed},
		{store.EdgeOpened, notify.KindAlertOpened, open},
	} {
		for i := range edge.conditions {
			n.dispatchOne(ctx, &edge.conditions[i], edge.name, edge.kind, rules, names, now)
		}
	}
}

func (n *Notifier) dispatchOne(
	ctx context.Context, c *store.AlertCondition, edge string, kind notify.Kind,
	rules []store.AlertRule, names map[string]string, now time.Time,
) {
	for i := range rules {
		r := &rules[i]
		if !alerts.Matches(r, c) {
			continue
		}
		// A rule inside its quiet window is left unclaimed, so the first scan after the window
		// ends sends the edge if it is still owed.
		if alerts.Quiet(r, now) {
			continue
		}
		claimed, err := n.DB.MarkNotified(ctx, c.ID, r.ID, edge, now)
		if err != nil {
			slog.ErrorContext(ctx, "claim alert notification",
				slog.String("condition_id", c.ID), slog.Any("error", err))
			continue
		}
		if !claimed {
			continue
		}
		n.EmitTo(ctx, alertEvent(c, kind, edge, names), r.ID, r.WebhookIDs)
	}
}

// alertEvent is the notification for one alert edge.
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

// summarize is the headline of one alert edge.
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
func (n *Notifier) instanceNames(ctx context.Context) map[string]string {
	instances, err := n.DB.ListInstances(ctx, nil)
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
