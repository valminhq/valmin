// Package delivery turns domain events and alert edges into durable webhook deliveries and
// runs the jobs that send them.
package delivery

import (
	"context"
	"database/sql"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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
	// Ready reports whether a running instance's log says its world has loaded. Nil counts every
	// running instance as loaded.
	Ready func(ctx context.Context, inst *store.Instance) bool

	// players is the join and leave cooldown NotifyGameEvent applies.
	players recentPlayers

	// mu guards life and stopped, which Run sets.
	mu      sync.Mutex
	life    context.Context
	stopped bool
	tasks   sync.WaitGroup
}

// Run gives the announcements started in the background a lifetime: it blocks until ctx is
// done, then waits for them, cancelled with it. Announcements owed after Run returns are
// dropped.
func (n *Notifier) Run(ctx context.Context) {
	n.mu.Lock()
	n.life = ctx
	n.mu.Unlock()
	<-ctx.Done()
	n.mu.Lock()
	n.stopped = true
	n.mu.Unlock()
	n.tasks.Wait()
}

// background runs fn apart from its caller: with ctx's values, cancelled only when the
// notifier's lifetime ends, or never before Run starts.
func (n *Notifier) background(ctx context.Context, fn func(context.Context)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return
	}
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := func() bool { return false }
	if n.life != nil {
		stop = context.AfterFunc(n.life, cancel) //nolint:contextcheck // the lifetime ends it, not the caller
	}
	n.tasks.Go(func() {
		defer cancel()
		defer stop()
		fn(detached)
	})
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
	n.emitByRules(ctx, alerts.KindPowerCut, "", func(p *alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindPowerCutSoon,
			Detail: []notify.Field{
				{Key: "power_off", Name: "Power goes off", Value: formatInstant(powerOff, p.Location())},
				{Key: "stop_at", Name: "Servers stop", Value: formatInstant(stopAt, p.Location())},
				{Key: "servers", Name: "Running servers", Value: servers},
			},
		}
	})
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
			Instances: instances, InstalledBuilds: instance.InstalledBuilds(instances),
			BranchBuilds: map[string]string{instance.PublicBranch: observed},
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
	case string(instance.StateRunning):
		status = "Running again"
		next = "Anyone playing was disconnected. Check the server's console log for why it went down."
	}
	return []notify.Field{
		{Name: "Status", Value: status},
		{Name: "Cause", Value: asSentence(reason)},
		{Name: "What to do", Value: next},
	}
}
