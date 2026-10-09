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
	"strconv"
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
	// JoinCode is a running instance's crossplay join code, "" while unknown. Nil never knows one.
	JoinCode func(instanceID string) string
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// OnJobFinished is the job engine's second finish hook. It hands a start or stop that succeeded
// to the lifecycle announcements, and owes a notification for a backup that failed, writing the
// delivery intents in the same transaction that makes the job terminal, so a notification is
// owed exactly when the failure it reports commits.
//
// The destinations are read through the reader pool rather than the caller's transaction: the
// write is what has to be atomic with the job's outcome, not the lookup of who to tell.
func (n *Notifier) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	n.announceLifecycle(ctx, fin)
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

// NotifyPowerCutSoon reports a planned power cut at powerOff, before which the servers named in
// running are stopped at stopAt, to the destinations of the power_cut rules.
func (n *Notifier) NotifyPowerCutSoon(ctx context.Context, powerOff, stopAt time.Time, running []string) {
	servers := strings.Join(running, ", ")
	n.emitByRules(ctx, alerts.KindPowerCut, "", func(p alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindPowerCutSoon,
			Detail: []notify.Field{
				{Name: "Power goes off", Value: formatInstant(powerOff, p.Location())},
				{Name: "Servers stop", Value: formatInstant(stopAt, p.Location())},
				{Name: "Running servers", Value: servers},
			},
		}
	})
}

// lifecycleWait bounds how long a start or stop announcement waits for the finish transaction to
// commit.
const lifecycleWait = 2 * time.Minute

// joinCodeWait bounds how long a started crossplay server is waited on for its join code.
//
// ponytail: a fixed ceiling in case a build never logs the code; measure how late it can come
// before lowering it.
const joinCodeWait = 15 * time.Minute

// announceLifecycle is OnJobFinished's share of a start, restart or stop that succeeded. It runs
// apart from the finish transaction, which holds the writer the deliveries need.
func (n *Notifier) announceLifecycle(ctx context.Context, fin *jobs.FinishedJob) {
	if fin.Status != jobs.StatusSucceeded || fin.InstanceID == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	switch fin.Kind {
	case jobs.KindStart, jobs.KindRestart:
		go n.NotifyServerStarted(ctx, *fin.InstanceID, joinCodeWait)
	case jobs.KindStop:
		payload, _ := fin.Payload.(instance.StopPayload)
		go n.NotifyServerStopped(ctx, *fin.InstanceID, payload.Reason, fin.RequestedBy, lifecycleWait)
	}
}

// NotifyServerStarted reports a server that reached running to the server_started rules
// covering it. Nobody can join a crossplay server before it logs its join code, which comes some
// time after readiness, so the message waits up to wait for the code and is sent without it only
// after that. A server that stops while waited on is not reported.
func (n *Notifier) NotifyServerStarted(ctx context.Context, instanceID string, wait time.Duration) {
	rules := n.rulesFor(ctx, alerts.KindServerStarted, instanceID)
	if len(rules) == 0 {
		return
	}
	var code string
	inst := n.awaitState(ctx, instanceID, instance.StateRunning, wait, func(inst *store.Instance) bool {
		if !inst.Crossplay {
			return true
		}
		code = n.joinCode(instanceID)
		return code != ""
	})
	if inst == nil {
		return
	}
	at := time.Now().UTC()
	n.emitByRules(ctx, alerts.KindServerStarted, instanceID, func(p alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindServerStarted, InstanceID: inst.ID, InstanceName: inst.Name,
			Detail: shown(p, []shownField{
				{alerts.FieldServerName, "Server name", inst.ServerName},
				{alerts.FieldWorld, "World", inst.WorldName},
				{alerts.FieldJoinCode, "Join code", code},
				{alerts.FieldPort, "Port", strconv.Itoa(inst.BasePort)},
				{alerts.FieldTime, "Started", formatInstant(at, p.Location())},
			}),
		}
	})
}

// NotifyServerStopped reports a server the panel stopped to the server_stopped rules covering it,
// with why: the reason the panel stopped it for, or else the user who asked.
func (n *Notifier) NotifyServerStopped(
	ctx context.Context, instanceID string, reason instance.StopReason, requestedBy string, wait time.Duration,
) {
	if len(n.rulesFor(ctx, alerts.KindServerStopped, instanceID)) == 0 {
		return
	}
	inst := n.awaitState(ctx, instanceID, instance.StateStopped, wait, nil)
	if inst == nil {
		return
	}
	why := n.stopReason(ctx, inst, reason, requestedBy)
	at := time.Now().UTC()
	n.emitByRules(ctx, alerts.KindServerStopped, instanceID, func(p alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindServerStopped, InstanceID: inst.ID, InstanceName: inst.Name,
			Detail: shown(p, []shownField{
				{alerts.FieldReason, "Reason", why},
				{alerts.FieldServerName, "Server name", inst.ServerName},
				{alerts.FieldWorld, "World", inst.WorldName},
				{alerts.FieldTime, "Stopped", formatInstant(at, p.Location())},
			}),
		}
	})
}

// stopReason says why a server was stopped, in words.
func (n *Notifier) stopReason(
	ctx context.Context, inst *store.Instance, reason instance.StopReason, requestedBy string,
) string {
	switch reason {
	case instance.StopNoPlayers:
		return fmt.Sprintf("No players for %d minutes", inst.AutoStopMinutes)
	case instance.StopPowerCut:
		return "Planned power cut"
	}
	if requestedBy != "" {
		u, err := n.DB.UserByID(ctx, requestedBy)
		if err != nil {
			slog.WarnContext(ctx, "read the user who stopped a server", slog.Any("error", err))
		}
		if u != nil {
			return "Stopped by " + u.Username
		}
	}
	return "Stopped from the panel"
}

// awaitState polls the instance until it is in state and ready accepts it, or wait passes. The
// instance is returned once it reached state even if ready never did, and nil if it never did or
// left state again before ready accepted it.
//
// ponytail: one-second polling per announcement; subscribe to state and join-code changes if
// starts ever come in bursts large enough to matter.
func (n *Notifier) awaitState(
	ctx context.Context, instanceID string, state instance.State, wait time.Duration,
	ready func(*store.Instance) bool,
) *store.Instance {
	deadline := time.Now().Add(wait)
	var reached *store.Instance
	for {
		inst, err := n.DB.InstanceByID(ctx, instanceID)
		if err != nil {
			slog.WarnContext(ctx, "read instance for a lifecycle notification",
				slog.String("instance_id", instanceID), slog.Any("error", err))
		}
		switch {
		case inst != nil && inst.State == string(state):
			reached = inst
			if ready == nil || ready(inst) {
				return inst
			}
		case inst != nil && reached != nil:
			return nil
		}
		if !time.Now().Before(deadline) {
			return reached
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (n *Notifier) joinCode(instanceID string) string {
	if n.JoinCode == nil {
		return ""
	}
	return n.JoinCode(instanceID)
}

type shownField struct{ id, name, value string }

// shown is the fields a rule has not hidden.
func shown(p alerts.ParamsWire, fields []shownField) []notify.Field {
	out := make([]notify.Field, 0, len(fields))
	for _, f := range fields {
		if p.Shows(f.id) {
			out = append(out, notify.Field{Name: f.name, Value: f.value})
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
	ctx context.Context, kind alerts.Kind, instanceID string, build func(alerts.ParamsWire) *notify.Event,
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
		event := build(alerts.ParamsWireOf(r.Params))
		event.ID, event.OccurredAt = eventID, now
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
