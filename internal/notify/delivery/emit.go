package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

type shownField struct{ id, name, value string }

// shown is the fields a rule has not hidden.
func shown(p *alerts.ParamsWire, fields []shownField) []notify.Field {
	out := make([]notify.Field, 0, len(fields))
	for _, f := range fields {
		if p.Shows(f.id) {
			out = append(out, notify.Field{Key: f.id, Name: f.name, Value: f.value})
		}
	}
	return out
}

// rulesFor is the enabled rules for kind covering instanceID. A read failure is logged and
// matches nothing.
func (n *Notifier) rulesFor(ctx context.Context, kind alerts.Kind, instanceID string) []store.AlertRule {
	rules, err := n.DB.ListAlertRules(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read alert rules",
			slog.String("rule_kind", kind.String()), slog.Any("error", err))
		return nil
	}
	condition := &store.AlertCondition{Kind: kind.String()}
	if instanceID != "" {
		condition.InstanceID = &instanceID
	}
	return slices.DeleteFunc(rules, func(r store.AlertRule) bool { return !alerts.Matches(&r, condition) })
}

// emitByRules sends the event build makes from each rule's params to the destinations of every
// enabled rule for kind that covers instanceID and is outside its quiet hours. A destination
// named by several rules is sent one, rendered for the first such rule.
func (n *Notifier) emitByRules(
	ctx context.Context, kind alerts.Kind, instanceID string, build func(*alerts.ParamsWire) *notify.Event,
) {
	eventID, now := store.NewID(), time.Now().UTC()
	sent := map[string]bool{}
	rules := n.rulesFor(ctx, kind, instanceID)
	for i := range rules {
		r := &rules[i]
		if alerts.Quiet(r, now) {
			continue
		}
		var targets []string
		for _, id := range r.WebhookIDs {
			if !sent[id] {
				sent[id] = true
				targets = append(targets, id)
			}
		}
		if len(targets) == 0 {
			continue
		}
		params := alerts.ParamsWireOf(r.Params)
		event := build(&params)
		event.ID, event.OccurredAt = eventID, now
		Compose(event, kind, &params)
		n.EmitTo(ctx, event, r.ID, targets)
	}
}

// Prepare renders one delivery row per enabled catch-all destination that serves the event's
// server. Nothing is sent by it: the rows are the delivery intent, and the caller writes them
// with the change that caused the event.
func (n *Notifier) Prepare(ctx context.Context, event *notify.Event) ([]*store.Delivery, error) {
	return n.render(ctx, event, func(w *store.Webhook) bool { return w.CatchAll })
}

// render is one delivery row for event per enabled destination that serves its server and
// that keep accepts.
func (n *Notifier) render(
	ctx context.Context, event *notify.Event, keep func(*store.Webhook) bool,
) ([]*store.Delivery, error) {
	n.link(event)
	destinations, err := n.DB.EnabledWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	var out []*store.Delivery
	for i := range destinations {
		if !keep(&destinations[i]) || !destinations[i].Serves(event.InstanceID) {
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

// PrepareFor renders one delivery row per named enabled destination, for an event a rule routes
// to a subset rather than to everyone. A named destination that is gone, disabled or another
// server's is skipped.
func (n *Notifier) PrepareFor(
	ctx context.Context, event *notify.Event, webhookIDs []string,
) ([]*store.Delivery, error) {
	if len(webhookIDs) == 0 {
		return nil, nil
	}
	return n.render(ctx, event, func(w *store.Webhook) bool { return slices.Contains(webhookIDs, w.ID) })
}

// EmitTo is Emit narrowed to the destinations a rule names.
func (n *Notifier) EmitTo(ctx context.Context, event *notify.Event, ruleID string, webhookIDs []string) {
	deliveries, err := n.ruleDeliveries(ctx, event, ruleID, webhookIDs)
	n.send(ctx, event, deliveries, err)
}

// send records the deliveries prepared for event and has the dispatcher send them. A failure is
// logged and nothing else: a notification never changes the outcome it reports.
func (n *Notifier) send(ctx context.Context, event *notify.Event, deliveries []*store.Delivery, err error) {
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

// editedDelivery is the id of eventID's delivery to webhookID, nil when there is none. A read
// failure costs the edit: the resolution is posted as a message of its own.
func (n *Notifier) editedDelivery(ctx context.Context, eventID, webhookID string) *string {
	if eventID == "" {
		return nil
	}
	d, err := n.DB.DeliveryForEvent(ctx, eventID, webhookID)
	if err != nil {
		slog.WarnContext(ctx, "read the delivery a resolution edits", slog.Any("error", err))
	}
	if d == nil {
		return nil
	}
	return &d.ID
}

// emitExcept sends event to every catch-all destination except those an alert rule owns.
func (n *Notifier) emitExcept(ctx context.Context, event *notify.Event, owned map[string]bool) {
	deliveries, err := n.prepareExcept(ctx, event, owned)
	n.send(ctx, event, deliveries, err)
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
