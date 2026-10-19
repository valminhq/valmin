package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/store"
)

// ruleOwned is the set of destinations an enabled alert rule will tell about the incident a v1
// event reports, so the v1 event can leave them out.
//
// snap describes the world as the incident leaves it, and only the named condition kinds count.
// A condition counts only if it is not open already: a rule announces a condition once, and an
// open one has normally been announced, so the v1 event is the only word the destination would
// get about this incident. A rule inside its quiet hours still owns its destinations; holding
// the alert until the window ends is the point of the window. A rule owns only destinations of
// its own scope: the shared ones for a rule about every server, the server's own for a rule
// about one server.
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
	ownerOf, err := n.destinationOwners(ctx)
	if err != nil {
		return nil, err
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
			claim(owned, &rules[i], ownerOf)
		}
	}
	return owned, nil
}

// destinationOwners maps each destination to the server it belongs to, "" for a shared one.
func (n *Notifier) destinationOwners(ctx context.Context) (map[string]string, error) {
	destinations, err := n.DB.ListWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("read destinations: %w", err)
	}
	ownerOf := make(map[string]string, len(destinations))
	for i := range destinations {
		ownerOf[destinations[i].ID] = deref(destinations[i].InstanceID)
	}
	return ownerOf, nil
}

// claim adds to owned the destinations of rule that belong to the rule's own scope.
func claim(owned map[string]bool, rule *store.AlertRule, ownerOf map[string]string) {
	for _, id := range rule.WebhookIDs {
		if owner, ok := ownerOf[id]; ok && owner == deref(rule.InstanceID) {
			owned[id] = true
		}
	}
}
