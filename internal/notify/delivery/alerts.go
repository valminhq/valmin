package delivery

import (
	"context"
	"log/slog"
	"strings"
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
		loc := alerts.ParamsWireOf(r.Params).Location()
		n.EmitTo(ctx, alertEvent(c, kind, edge, names, loc), r.ID, r.WebhookIDs)
	}
}

// alertEvent is the notification for one alert edge.
func alertEvent(
	c *store.AlertCondition, kind notify.Kind, edge string, names map[string]string, loc *time.Location,
) *notify.Event {
	e := &notify.Event{
		ID:         store.NewID(),
		Kind:       kind,
		OccurredAt: time.Now().UTC(),
		Summary:    summarize(c, edge),
		Detail:     conditionDetail(c, edge, loc),
	}
	if c.InstanceID != nil {
		e.InstanceID = *c.InstanceID
		e.InstanceName = names[*c.InstanceID]
	}
	return e
}

// summarize is the headline of one alert edge. The server is named beside the headline rather
// than in it, so a sentence reads the same for every server.
func summarize(c *store.AlertCondition, edge string) string {
	job := jobLabel(c.Detail["Job"])
	var sentence string
	switch c.Kind {
	case alerts.KindJobFailed.String():
		sentence = job + " failed"
	case alerts.KindLowDisk.String():
		sentence = "The host is running low on disk space"
	case alerts.KindStaleBackup.String():
		sentence = "Scheduled backups have stopped"
	case alerts.KindUncleanStop.String():
		sentence = "The server stopped before its world finished saving"
	case alerts.KindRestartRequired.String():
		sentence = "The server needs a restart to apply a change"
	case alerts.KindUpdateAvailable.String():
		sentence = "A game update is available"
	case alerts.KindInstanceError.String():
		sentence = "The server needs a check after a failure"
	case alerts.KindCrashLoop.String():
		sentence = "The server keeps crashing"
	case alerts.KindJobStuck.String():
		sentence = job + " is taking unusually long"
	default:
		sentence = upperFirst(strings.ReplaceAll(c.Kind, "_", " "))
	}
	if edge == store.EdgeResolved {
		return "Resolved: " + sentence
	}
	return sentence
}

// conditionDetail is the readable form of a condition's stored detail, which keeps raw values
// for the inbox to format. An opening edge also says what to do; a resolution has nothing left
// to do.
func conditionDetail(c *store.AlertCondition, edge string, loc *time.Location) []notify.Field {
	d := c.Detail
	var fields []notify.Field
	var next string
	switch c.Kind {
	case alerts.KindJobFailed.String():
		fields = []notify.Field{{Name: "Reason", Value: failureReason(d["Reason"])}}
		next = "Open the server's job history for the full log, then try the job again."
	case alerts.KindLowDisk.String():
		fields = []notify.Field{
			{Name: "Free space", Value: formatBytes(d["Free"])},
			{Name: "Alert below", Value: formatBytes(d["Alarm"])},
		}
		next = "Delete old backups, worlds or saved setups, or free space on the host. " +
			"Backups and updates fail once the disk is full."
	case alerts.KindStaleBackup.String():
		last := "None on record"
		if d["Last"] != "" {
			last = formatTime(d["Last"], loc)
		}
		fields = []notify.Field{
			{Name: "Schedule", Value: "Every " + formatDuration(d["Every"])},
			{Name: "Last backup", Value: last},
		}
		next = "Check the server's recent backup jobs for failures."
	case alerts.KindUncleanStop.String():
		fields = []notify.Field{{Name: "Job", Value: jobLabel(d["Job"])}}
		next = "Recent progress may be lost. Check the world before players rejoin, " +
			"and restore a backup if it is damaged."
	case alerts.KindRestartRequired.String():
		next = "Restart the server when no one is playing."
	case alerts.KindUpdateAvailable.String():
		fields = []notify.Field{
			{Name: "Installed build", Value: d["Installed"]},
			{Name: "Available build", Value: d["Available"]},
		}
		next = "Stop the server, then update it from its page in Valmin."
	case alerts.KindInstanceError.String():
		next = "Open the server in Valmin and choose Check this server. Its last failed job has the cause."
	case alerts.KindCrashLoop.String():
		fields = []notify.Field{{
			Name:  "Unexpected stops",
			Value: d["Stops"] + " in the last " + formatDuration(d["Window"]),
		}}
		next = "Check the server's console log for the error that stops it."
	case alerts.KindJobStuck.String():
		fields = []notify.Field{{Name: "Running for", Value: formatDuration(d["Running"])}}
		next = "Check the job's progress in Valmin, and cancel it if it is not moving."
	}
	if edge != store.EdgeResolved && next != "" {
		fields = append(fields, notify.Field{Name: "What to do", Value: next})
	}
	return fields
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
