package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// Notifier prepares durable delivery intents for domain events.
type Notifier struct {
	DB         *store.DB
	Dispatcher *Dispatcher
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
func (h *Notifier) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
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
		Detail:       map[string]string{"Job": fin.ID},
	}
	owned := h.ruleOwned(ctx, &alerts.Snapshot{LatestTerminalJobs: []store.Job{{
		ID: fin.ID, Kind: fin.Kind.String(), Status: jobs.StatusFailed, InstanceID: fin.InstanceID,
	}}}, alerts.KindJobFailed)
	deliveries, err := h.prepareExcept(ctx, event, owned)
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
func (h *Notifier) NotifyUnexpectedStop(ctx context.Context, inst *store.Instance, to, reason string) {
	now := time.Now().UTC()
	// The row as the observer is about to leave it, which is what instance_error reads.
	after := *inst
	after.State = to
	snap := &alerts.Snapshot{Instances: []store.Instance{after}, Now: now}
	// The incident is already recorded, so a crash loop this stop completes is visible here.
	incidents, err := h.DB.RecentIncidents(ctx, now.Add(-alerts.IncidentRetention))
	if err != nil {
		slog.WarnContext(ctx, "read recent incidents", slog.Any("error", err))
	}
	snap.Incidents = incidents
	owned := h.ruleOwned(ctx, snap, alerts.KindCrashLoop, alerts.KindInstanceError)

	h.emitExcept(ctx, &notify.Event{
		ID:           store.NewID(),
		Kind:         notify.KindInstanceDown,
		OccurredAt:   now,
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		Detail:       map[string]string{"State": to, "Observed": reason},
	}, owned)
}

// NotifyPublicBuild owes a notification when the observed public build is one the panel has
// not seen before. An unchanged observation is the common case — the check runs hourly — and
// says nothing, so a receiver is told about a new build once rather than every hour until
// someone updates (05 M6).
func (h *Notifier) NotifyPublicBuild(
	ctx context.Context, previous, observed string,
) func(context.Context, *sql.Tx) error {
	if observed == "" || observed == previous {
		return nil
	}
	event := &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindUpdateAvailable,
		OccurredAt: time.Now().UTC(),
		Detail:     map[string]string{"Build": observed},
	}
	var owned map[string]bool
	if instances, err := h.DB.ListInstances(ctx, nil); err != nil {
		slog.WarnContext(ctx, "read instances for update-available routing", slog.Any("error", err))
	} else {
		owned = h.ruleOwned(ctx, &alerts.Snapshot{
			Instances: instances, InstalledBuilds: alerts.InstalledBuilds(instances), PublicBuild: observed,
		}, alerts.KindUpdateAvailable)
	}
	deliveries, err := h.prepareExcept(ctx, event, owned)
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
func (h *Notifier) Prepare(ctx context.Context, event *notify.Event) ([]*store.Delivery, error) {
	destinations, err := h.DB.EnabledWebhooks(ctx)
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
func (h *Notifier) PrepareFor(
	ctx context.Context, event *notify.Event, webhookIDs []string,
) ([]*store.Delivery, error) {
	if len(webhookIDs) == 0 {
		return nil, nil
	}
	wanted := make(map[string]bool, len(webhookIDs))
	for _, id := range webhookIDs {
		wanted[id] = true
	}
	destinations, err := h.DB.EnabledWebhooks(ctx)
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
func (h *Notifier) EmitTo(ctx context.Context, event *notify.Event, ruleID string, webhookIDs []string) {
	deliveries, err := h.PrepareFor(ctx, event, webhookIDs)
	if err != nil {
		slog.ErrorContext(ctx, "prepare notification",
			slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
		return
	}
	for _, d := range deliveries {
		d.RuleID = &ruleID
		if err := h.DB.CreateDelivery(ctx, d); err != nil {
			slog.ErrorContext(ctx, "record delivery intent",
				slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
			return
		}
	}
	h.Dispatcher.Send(ctx, deliveries)
}

// Emit is the path for an event whose source change is not a transaction this package holds:
// the rows are written and the dispatcher sends them. A failure is logged and nothing else —
// no notification ever changes the outcome it reports (05 M6).
func (h *Notifier) Emit(ctx context.Context, event *notify.Event) {
	h.emitExcept(ctx, event, nil)
}

// emitExcept is Emit without the destinations an alert rule owns (notifications.go).
func (h *Notifier) emitExcept(ctx context.Context, event *notify.Event, owned map[string]bool) {
	deliveries, err := h.prepareExcept(ctx, event, owned)
	if err != nil {
		slog.ErrorContext(ctx, "prepare notification",
			slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
		return
	}
	for _, d := range deliveries {
		if err := h.DB.CreateDelivery(ctx, d); err != nil {
			slog.ErrorContext(ctx, "record delivery intent",
				slog.String("event_kind", event.Kind.String()), slog.Any("error", err))
			return
		}
	}
	h.Dispatcher.Send(ctx, deliveries)
}

// prepareExcept is Prepare without the destinations a rule owns.
func (h *Notifier) prepareExcept(
	ctx context.Context, event *notify.Event, owned map[string]bool,
) ([]*store.Delivery, error) {
	deliveries, err := h.Prepare(ctx, event)
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
func (h *Notifier) ruleOwned(ctx context.Context, snap *alerts.Snapshot, kinds ...alerts.Kind) map[string]bool {
	owned, err := h.ruleOwnedErr(ctx, snap, kinds)
	if err != nil {
		slog.WarnContext(ctx, "route a notification through the alert rules; "+
			"sending it to every destination", slog.Any("error", err))
		return nil
	}
	return owned
}

func (h *Notifier) ruleOwnedErr(
	ctx context.Context, snap *alerts.Snapshot, kinds []alerts.Kind,
) (map[string]bool, error) {
	rules, err := h.DB.ListAlertRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("read alert rules: %w", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	open, err := h.DB.OpenConditions(ctx)
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
