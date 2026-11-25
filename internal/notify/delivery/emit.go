package delivery

import (
	"cmp"
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

// rulesFor is the enabled rules for kind covering instanceID, the rules naming a server before
// the rules for every server. A read failure is logged and matches nothing.
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
	rules = slices.DeleteFunc(rules, func(r store.AlertRule) bool { return !alerts.Matches(&r, condition) })
	slices.SortStableFunc(rules, func(a, b store.AlertRule) int {
		return cmp.Compare(boolRank(a.InstanceID == nil), boolRank(b.InstanceID == nil))
	})
	return rules
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emitByRules sends the deliveries prepareByRules makes and records them as it goes.
func (n *Notifier) emitByRules(
	ctx context.Context, kind alerts.Kind, instanceID string, build func(*alerts.ParamsWire) *notify.Event,
) {
	for _, d := range n.prepareByRules(ctx, kind, instanceID, build) {
		n.send(ctx, d.event, d.deliveries)
	}
}

// ruleEvent is one rule's rendering of an event and its unrecorded delivery rows.
type ruleEvent struct {
	event      *notify.Event
	deliveries []*store.Delivery
}

// prepareByRules renders the event build makes from each rule's params for the destinations of
// every enabled rule for kind that covers instanceID and is outside its quiet hours. Nothing is
// recorded or sent. A destination named by several rules gets one, rendered for the first such
// rule, so a server's own rule wins over a rule for every server. A failure to render is logged
// and costs that rule's deliveries.
func (n *Notifier) prepareByRules(
	ctx context.Context, kind alerts.Kind, instanceID string, build func(*alerts.ParamsWire) *notify.Event,
) []ruleEvent {
	eventID, now := store.NewID(), time.Now().UTC()
	sent := map[string]bool{}
	var out []ruleEvent
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
		deliveries, err := n.ruleDeliveries(ctx, event, r.ID, targets)
		if err != nil {
			slog.ErrorContext(ctx, "prepare notification",
				slog.String("event_kind", event.Kind.String()), slog.String("rule_id", r.ID),
				slog.Any("error", err))
			continue
		}
		out = append(out, ruleEvent{event: event, deliveries: deliveries})
	}
	return out
}

// PrepareFor renders one delivery row per named enabled destination that serves the event's
// server. A named destination that is gone, disabled or another server's is skipped. Nothing is
// sent by it: the rows are the delivery intent, which the caller records.
func (n *Notifier) PrepareFor(
	ctx context.Context, event *notify.Event, webhookIDs []string,
) ([]*store.Delivery, error) {
	if len(webhookIDs) == 0 {
		return nil, nil
	}
	n.link(event)
	destinations, err := n.DB.EnabledWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	var out []*store.Delivery
	for i := range destinations {
		if !slices.Contains(webhookIDs, destinations[i].ID) || !destinations[i].Serves(event.InstanceID) {
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

// send records the deliveries prepared for event and has the dispatcher send them. A failure is
// logged and nothing else: a notification never changes the outcome it reports.
func (n *Notifier) send(ctx context.Context, event *notify.Event, deliveries []*store.Delivery) {
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
