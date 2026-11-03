package delivery

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
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
		params := alerts.ParamsWireOf(r.Params)
		event := alertEvent(c, kind, edge, names, params.Location())
		if edge == store.EdgeResolved {
			opened, err := n.DB.NotifiedEvent(ctx, c.ID, r.ID, store.EdgeOpened)
			if err != nil {
				slog.WarnContext(ctx, "read the alert a resolution edits",
					slog.String("condition_id", c.ID), slog.Any("error", err))
			}
			event.Resolves = opened
		}
		ruleKind, _ := alerts.ParseKind(r.ConditionKind)
		Compose(event, ruleKind, &params)
		deliveries, err := n.ruleDeliveries(ctx, event, r.ID, r.WebhookIDs)
		if err != nil {
			slog.ErrorContext(ctx, "prepare alert notification",
				slog.String("condition_id", c.ID), slog.Any("error", err))
			continue
		}
		claimed, err := n.DB.MarkNotified(ctx, &store.Notification{
			ConditionID: c.ID, RuleID: r.ID, Edge: edge, EventID: event.ID,
		}, now, deliveries)
		if err != nil {
			slog.ErrorContext(ctx, "claim alert notification",
				slog.String("condition_id", c.ID), slog.Any("error", err))
			continue
		}
		if claimed {
			n.Dispatcher.Send(ctx, deliveries)
		}
	}
}

// ruleDeliveries are the unrecorded delivery rows of event to the webhooks rule ruleID names.
// Each row records the rule, so a rule's deliveries can be listed, and a row of a resolution
// names the delivery of the alert it resolves, whose message it edits.
func (n *Notifier) ruleDeliveries(
	ctx context.Context, event *notify.Event, ruleID string, webhookIDs []string,
) ([]*store.Delivery, error) {
	deliveries, err := n.PrepareFor(ctx, event, webhookIDs)
	if err != nil {
		return nil, err
	}
	for _, d := range deliveries {
		d.RuleID = &ruleID
		d.EditsID = n.editedDelivery(ctx, event.Resolves, d.WebhookID)
	}
	return deliveries, nil
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

// sentences is the headline of each condition kind whose headline names no job.
var sentences = map[alerts.Kind]string{
	alerts.KindLowDisk:         "The host is running low on disk space",
	alerts.KindStaleBackup:     "Scheduled backups have stopped",
	alerts.KindUncleanStop:     "The server stopped before its world finished saving",
	alerts.KindRestartRequired: "The server needs a restart to apply a change",
	alerts.KindUpdateAvailable: "A game update is available",
	alerts.KindInstanceError:   "The server needs a check after a failure",
	alerts.KindCrashLoop:       "The server keeps crashing",
	alerts.KindBackupAttention: "The newest backup's world needs a look",
	alerts.KindModsFailed:      "Mods failed to load",
	alerts.KindModUpdates:      "Mod updates are available",
}

// summarize is the headline of one alert edge. The server is named beside the headline rather
// than in it, so a sentence reads the same for every server.
func summarize(c *store.AlertCondition, edge string) string {
	job := jobLabel(c.Detail[alerts.DetailJob])
	kind, _ := alerts.ParseKind(c.Kind)
	sentence, ok := sentences[kind]
	switch {
	case kind == alerts.KindJobFailed:
		sentence = job + " failed"
	case kind == alerts.KindJobStuck:
		sentence = job + " is taking unusually long"
	case !ok:
		sentence = upperFirst(strings.ReplaceAll(c.Kind, "_", " "))
	}
	if edge != store.EdgeResolved {
		return sentence
	}
	if c.ResolvedAt != nil {
		if lasted := c.ResolvedAt.Sub(c.FirstSeenAt); lasted >= time.Minute {
			return "Resolved after " + formatDuration(lasted.String()) + ": " + sentence
		}
	}
	return "Resolved: " + sentence
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
		fields = []notify.Field{{Key: "reason", Name: "Reason", Value: failureReason(d[alerts.DetailReason])}}
		next = "Open the server's job history for the full log, then try the job again."
	case alerts.KindLowDisk.String():
		fields = []notify.Field{
			{Key: "free", Name: "Free space", Value: formatBytes(d[alerts.DetailFree])},
			{Key: "alarm", Name: "Alert below", Value: formatBytes(d[alerts.DetailAlarm])},
		}
		next = "Delete old backups, worlds or saved setups, or free space on the host. " +
			"Backups and updates fail once the disk is full."
	case alerts.KindStaleBackup.String():
		last := "None on record"
		if d[alerts.DetailLast] != "" {
			last = formatTime(d[alerts.DetailLast], loc)
		}
		fields = []notify.Field{
			{Key: "schedule", Name: "Schedule", Value: "Every " + formatDuration(d[alerts.DetailEvery])},
			{Key: "last_backup", Name: "Last backup", Value: last},
		}
		next = "Check the server's recent backup jobs for failures."
	case alerts.KindUncleanStop.String():
		fields = []notify.Field{{Key: "job", Name: "Job", Value: jobLabel(d[alerts.DetailJob])}}
		next = "Recent progress may be lost. Check the world before players rejoin, " +
			"and restore a backup if it is damaged."
	case alerts.KindRestartRequired.String():
		next = "Restart the server when no one is playing."
	case alerts.KindUpdateAvailable.String():
		fields = []notify.Field{
			{Key: "installed", Name: "Installed build", Value: d[alerts.DetailInstalled]},
			{Key: "available", Name: "Available build", Value: d[alerts.DetailAvailable]},
		}
		next = "Stop the server, then update it from its page in Valmin."
	case alerts.KindInstanceError.String():
		fields, next = errorDetail(d)
	case alerts.KindCrashLoop.String():
		fields = []notify.Field{{
			Key:   "stops",
			Name:  "Unexpected stops",
			Value: d[alerts.DetailStops] + " in the last " + formatDuration(d[alerts.DetailWindow]),
		}}
		next = "Check the server's console log for the error that stops it."
	case alerts.KindJobStuck.String():
		fields = []notify.Field{{Key: "running", Name: "Running for", Value: formatDuration(d[alerts.DetailRunning])}}
		next = "Check the job's progress in Valmin, and cancel it if it is not moving."
	case alerts.KindBackupAttention.String():
		fields = []notify.Field{
			{Key: "backup", Name: "Backup", Value: formatTime(d[alerts.DetailBackup], loc)},
			{Key: "findings", Name: "Found", Value: d[alerts.DetailReason]},
		}
		next = "Check the server log around the time of that backup. If the world is damaged, " +
			"restore the newest backup without a warning; if the change was intended, " +
			"acknowledge the warning on the Backups page."
	case alerts.KindModsFailed.String(), alerts.KindModUpdates.String():
		fields, next = modDetail(c.Kind, d)
	}
	if edge != store.EdgeResolved && next != "" {
		fields = append(fields, notify.Field{Key: "what_to_do", Name: "What to do", Value: next})
	}
	return fields
}

// errorDetail is the cause and next step of a server parked in error. The observer's reason is
// the cause when there is one; otherwise the server's last failed job has it.
func errorDetail(d map[string]string) (fields []notify.Field, next string) {
	reason := d[alerts.DetailReason]
	if reason == "" {
		return nil, "Open the server in Valmin and choose Check this server. Its last failed job has the cause."
	}
	limit, _ := strconv.Atoi(d[alerts.DetailMemLimit])
	fields = []notify.Field{{Key: "reason", Name: "Cause", Value: stopCause(reason, limit)}}
	if reason == instance.ReasonOOMKilled {
		return fields, memoryAdvice(limit)
	}
	return fields, "Open the server in Valmin and choose Check this server."
}

// modDetail is the fields and next step of a mod condition: mods that failed to load, or mod
// updates waiting.
func modDetail(kind string, d map[string]string) (fields []notify.Field, next string) {
	if kind == alerts.KindModsFailed.String() {
		return []notify.Field{
			{Key: "mods", Name: "Mods", Value: d[alerts.DetailMods]},
			{Key: "reason", Name: "Reason", Value: d[alerts.DetailReason]},
		}, "Open the server's Mods page, update or disable the named mods, then restart the server."
	}
	return []notify.Field{
		{Key: "count", Name: "Updates", Value: d[alerts.DetailCount]},
		{Key: "mods", Name: "Mods", Value: d[alerts.DetailMods]},
	}, "Review the updates on the server's Mods page. Test them on a copy first if friends are playing."
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
