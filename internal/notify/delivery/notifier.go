// Package delivery turns domain events and alert edges into durable webhook deliveries and
// runs the jobs that send them.
package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// Notifier prepares durable delivery intents for domain events.
type Notifier struct {
	DB         *store.DB
	Dispatcher *Dispatcher
	// ExternalURL is the panel's own address, which a notification links back to. Empty sends
	// no link.
	ExternalURL string
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// OnJobFinished is the job engine's second finish hook. It owes a notification for a backup
// that failed, and writes the delivery intents in the same transaction that makes the job
// terminal, so a notification is owed exactly when the failure it reports commits.
//
// The destinations are read through the reader pool rather than the caller's transaction: the
// write is what has to be atomic with the job's outcome, not the lookup of who to tell.
func (n *Notifier) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	if fin.Kind != jobs.KindBackup || fin.Status != jobs.StatusFailed {
		return nil
	}
	event := &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindBackupFailed,
		OccurredAt: time.Now().UTC(),
		InstanceID: deref(fin.InstanceID),
		// Named from the job's own submission, so nothing here reads a row inside the
		// finish transaction.
		InstanceName: fin.InstanceName,
		Detail: []notify.Field{
			{Name: "Reason", Value: failureReason(fin.Error)},
			{Name: "What to do", Value: "Open the server's job history for the full log, then run the backup again."},
			{Name: "Job ID", Value: fin.ID},
		},
	}
	owned := n.ruleOwned(ctx, &alerts.Snapshot{LatestTerminalJobs: []store.Job{{
		ID: fin.ID, Kind: fin.Kind.String(), Status: jobs.StatusFailed, InstanceID: fin.InstanceID,
	}}}, alerts.KindJobFailed)
	deliveries, err := n.prepareExcept(ctx, event, owned)
	if err != nil {
		slog.ErrorContext(ctx, "prepare backup-failed notification", slog.Any("error", err))
		return nil
	}
	if err := Record(ctx, tx, deliveries); err != nil {
		slog.ErrorContext(ctx, "record backup-failed notification", slog.Any("error", err))
	}
	return nil
}

// NotifyUnexpectedStop is what the observer calls when it finds a server that left `running`
// with no job holding its lock (C14). An operator's own stop holds that lock, so an expected
// stop never reaches here — the quiet is structural rather than a flag this has to read.
//
// The state write has already committed. A crash in the gap costs this one notification, which
// is the trade for keeping the observer's write path free of a notification's transaction.
func (n *Notifier) NotifyUnexpectedStop(ctx context.Context, inst *store.Instance, to, reason string) {
	now := time.Now().UTC()
	// The row as the observer is about to leave it, which is what instance_error reads.
	after := *inst
	after.State = to
	snap := &alerts.Snapshot{Instances: []store.Instance{after}, Now: now}
	// The incident is already recorded, so a crash loop this stop completes is visible here.
	incidents, err := n.DB.RecentIncidents(ctx, now.Add(-alerts.IncidentRetention))
	if err != nil {
		slog.WarnContext(ctx, "read recent incidents", slog.Any("error", err))
	}
	snap.Incidents = incidents
	owned := n.ruleOwned(ctx, snap, alerts.KindCrashLoop, alerts.KindInstanceError)

	n.emitExcept(ctx, &notify.Event{
		ID:           store.NewID(),
		Kind:         notify.KindInstanceDown,
		OccurredAt:   now,
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		Detail:       downDetail(to, reason),
	}, owned)
}

// NotifyAutoStopped reports a stop auto-stop submitted for a server that had no players, to the
// destinations of the auto_stopped rules covering it.
func (n *Notifier) NotifyAutoStopped(ctx context.Context, inst *store.Instance) {
	n.emitByRules(ctx, alerts.KindAutoStopped, inst.ID, &notify.Event{
		ID:           store.NewID(),
		Kind:         notify.KindInstanceAutoStopped,
		OccurredAt:   time.Now().UTC(),
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		Detail: []notify.Field{{
			Name: "Idle for", Value: fmt.Sprintf("%d minutes", inst.AutoStopMinutes),
		}},
	})
}

// NotifyPowerCutSoon reports a planned power cut at powerOff, before which the servers named in
// running are stopped at stopAt, to the destinations of the power_cut rules.
func (n *Notifier) NotifyPowerCutSoon(ctx context.Context, powerOff, stopAt time.Time, running []string) {
	servers := strings.Join(running, ", ")
	const layout = "2006-01-02 15:04 MST"
	n.emitByRules(ctx, alerts.KindPowerCut, "", &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindPowerCutSoon,
		OccurredAt: time.Now().UTC(),
		Detail: []notify.Field{
			{Name: "Power goes off", Value: powerOff.UTC().Format(layout)},
			{Name: "Servers stop", Value: stopAt.UTC().Format(layout)},
			{Name: "Running servers", Value: servers},
		},
	})
}

// emitByRules sends event to the destinations of every enabled rule for kind that covers
// instanceID and is outside its quiet hours. A destination named by several rules is sent one.
func (n *Notifier) emitByRules(ctx context.Context, kind alerts.Kind, instanceID string, event *notify.Event) {
	rules, err := n.DB.ListAlertRules(ctx)
	if err != nil {
		slog.ErrorContext(
			ctx,
			"read alert rules",
			slog.String("event_kind", event.Kind.String()),
			slog.Any("error", err),
		)
		return
	}
	condition := &store.AlertCondition{Kind: kind.String()}
	if instanceID != "" {
		condition.InstanceID = &instanceID
	}
	sent := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		if !alerts.Matches(r, condition) || alerts.Quiet(r, event.OccurredAt) {
			continue
		}
		var targets []string
		for _, id := range r.WebhookIDs {
			if !sent[id] {
				sent[id] = true
				targets = append(targets, id)
			}
		}
		n.EmitTo(ctx, event, r.ID, targets)
	}
}

// NotifyPublicBuild owes a notification when the observed public build is one the panel has
// not seen before. An unchanged observation is the common case — the check runs hourly — and
// says nothing, so a receiver is told about a new build once rather than every hour until
// someone updates (05 M6).
func (n *Notifier) NotifyPublicBuild(
	ctx context.Context, previous, observed string,
) func(context.Context, *sql.Tx) error {
	if observed == "" || observed == previous {
		return nil
	}
	event := &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindUpdateAvailable,
		OccurredAt: time.Now().UTC(),
		Detail: []notify.Field{
			{Name: "Available build", Value: observed},
			{Name: "What to do", Value: "Stop each server, then update it from its page in Valmin."},
		},
	}
	var owned map[string]bool
	if instances, err := n.DB.ListInstances(ctx, nil); err != nil {
		slog.WarnContext(ctx, "read instances for update-available routing", slog.Any("error", err))
	} else {
		owned = n.ruleOwned(ctx, &alerts.Snapshot{
			Instances: instances, InstalledBuilds: instance.InstalledBuilds(instances), PublicBuild: observed,
		}, alerts.KindUpdateAvailable)
	}
	deliveries, err := n.prepareExcept(ctx, event, owned)
	if err != nil {
		slog.ErrorContext(ctx, "prepare update-available notification", slog.Any("error", err))
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := Record(ctx, tx, deliveries); err != nil {
			slog.ErrorContext(ctx, "record update-available notification", slog.Any("error", err))
		}
		return nil
	}
}

// Prepare renders one delivery row per enabled destination. Nothing is sent by it: the rows
// are the delivery intent, and the caller writes them with the change that caused the event.
func (n *Notifier) Prepare(ctx context.Context, event *notify.Event) ([]*store.Delivery, error) {
	n.link(event)
	destinations, err := n.DB.EnabledWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	out := make([]*store.Delivery, 0, len(destinations))
	for i := range destinations {
		delivery, err := Render(&destinations[i], event)
		if err != nil {
			return nil, err
		}
		out = append(out, delivery)
	}
	return out, nil
}

// PrepareFor renders one delivery row per named enabled destination, for an event a rule routes
// to a subset rather than to everyone. A named destination that is gone or disabled is skipped.
func (n *Notifier) PrepareFor(
	ctx context.Context, event *notify.Event, webhookIDs []string,
) ([]*store.Delivery, error) {
	if len(webhookIDs) == 0 {
		return nil, nil
	}
	n.link(event)
	wanted := make(map[string]bool, len(webhookIDs))
	for _, id := range webhookIDs {
		wanted[id] = true
	}
	destinations, err := n.DB.EnabledWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	out := make([]*store.Delivery, 0, len(webhookIDs))
	for i := range destinations {
		if !wanted[destinations[i].ID] {
			continue
		}
		delivery, err := Render(&destinations[i], event)
		if err != nil {
			return nil, err
		}
		out = append(out, delivery)
	}
	return out, nil
}

// EmitTo is Emit narrowed to the destinations a rule names. Each row records the rule, so a
// rule's deliveries can be listed.
func (n *Notifier) EmitTo(ctx context.Context, event *notify.Event, ruleID string, webhookIDs []string) {
	deliveries, err := n.PrepareFor(ctx, event, webhookIDs)
	if err != nil {
		slog.ErrorContext(ctx, "prepare notification",
			slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
		return
	}
	for _, d := range deliveries {
		d.RuleID = &ruleID
		if err := n.DB.CreateDelivery(ctx, d); err != nil {
			slog.ErrorContext(ctx, "record delivery intent",
				slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
			return
		}
	}
	n.Dispatcher.Send(ctx, deliveries)
}

// emitExcept writes the delivery rows for event, except to the destinations an alert rule
// owns, and has the dispatcher send them. A failure is logged and nothing else: a notification
// never changes the outcome it reports.
func (n *Notifier) emitExcept(ctx context.Context, event *notify.Event, owned map[string]bool) {
	deliveries, err := n.prepareExcept(ctx, event, owned)
	if err != nil {
		slog.ErrorContext(ctx, "prepare notification",
			slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
		return
	}
	for _, d := range deliveries {
		if err := n.DB.CreateDelivery(ctx, d); err != nil {
			slog.ErrorContext(ctx, "record delivery intent",
				slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
			return
		}
	}
	n.Dispatcher.Send(ctx, deliveries)
}

// prepareExcept is Prepare without the destinations a rule owns.
func (n *Notifier) prepareExcept(
	ctx context.Context, event *notify.Event, owned map[string]bool,
) ([]*store.Delivery, error) {
	deliveries, err := n.Prepare(ctx, event)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(deliveries, func(d *store.Delivery) bool { return owned[d.WebhookID] }), nil
}

// ruleOwned is the set of destinations an enabled alert rule will tell about the incident a v1
// event reports, so the v1 event can leave them out.
//
// snap describes the world as the incident leaves it, and only the named condition kinds count.
// A condition counts only if it is not open already: a rule announces a condition once, and an
// open one has normally been announced, so the v1 event is the only word the destination would
// get about this incident. A rule inside its quiet hours still owns its destinations; holding
// the alert until the window ends is the point of the window.
//
// A read failure owns nothing. A duplicate alert is the lesser fault than a missing one.
func (n *Notifier) ruleOwned(ctx context.Context, snap *alerts.Snapshot, kinds ...alerts.Kind) map[string]bool {
	owned, err := n.ruleOwnedErr(ctx, snap, kinds)
	if err != nil {
		slog.WarnContext(ctx, "route a notification through the alert rules; "+
			"sending it to every destination", slog.Any("error", err))
		return nil
	}
	return owned
}

func (n *Notifier) ruleOwnedErr(
	ctx context.Context, snap *alerts.Snapshot, kinds []alerts.Kind,
) (map[string]bool, error) {
	rules, err := n.DB.ListAlertRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("read alert rules: %w", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	open, err := n.DB.OpenConditions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read open conditions: %w", err)
	}
	isOpen := make(map[string]bool, len(open))
	for i := range open {
		isOpen[open[i].Kind+"\x00"+deref(open[i].InstanceID)] = true
	}
	if snap.Now.IsZero() {
		snap.Now = time.Now().UTC()
	}

	owned := map[string]bool{}
	for _, c := range alerts.Evaluate(snap, alerts.RuleResolver(rules)) {
		if !slices.Contains(kinds, c.Kind) || isOpen[c.Kind.String()+"\x00"+c.InstanceID] {
			continue
		}
		condition := store.AlertCondition{Kind: c.Kind.String()}
		if c.InstanceID != "" {
			id := c.InstanceID
			condition.InstanceID = &id
		}
		for i := range rules {
			if !alerts.Matches(&rules[i], &condition) {
				continue
			}
			for _, id := range rules[i].WebhookIDs {
				owned[id] = true
			}
		}
	}
	return owned, nil
}

// link points the event at the panel page it is about: the server's own page, or the panel's
// front page for a host-wide event.
func (n *Notifier) link(event *notify.Event) {
	if n.ExternalURL == "" || event.URL != "" {
		return
	}
	base := strings.TrimSuffix(n.ExternalURL, "/")
	if event.InstanceID != "" {
		event.URL = base + "/instances/" + url.PathEscape(event.InstanceID)
		return
	}
	event.URL = base + "/"
}

// failureReason is a failed job's recorded error as a sentence, or a plain admission that none
// was recorded.
func failureReason(recorded string) string {
	if reason := asSentence(recorded); reason != "" {
		return reason
	}
	return "Valmin did not record a reason."
}

// downDetail explains a server that went down on its own: where it is now, the cause the
// observer saw, and what the reader can do about it.
func downDetail(to, reason string) []notify.Field {
	status, next := to, "Open the server in Valmin to see its current state."
	switch to {
	case string(instance.StateStopped):
		status = "Stopped"
		next = "Check the server's console log for why it exited, then start it again."
	case string(instance.StateError):
		status = "Held in the error state; its controls are locked until someone checks it"
		next = "Open the server in Valmin and choose Check this server."
	}
	return []notify.Field{
		{Name: "Status", Value: status},
		{Name: "Cause", Value: asSentence(reason)},
		{Name: "What to do", Value: next},
	}
}
