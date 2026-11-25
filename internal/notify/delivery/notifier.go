// Package delivery turns domain events and alert edges into durable webhook deliveries and
// runs the jobs that send them.
package delivery

import (
	"context"
	"database/sql"
	"fmt"
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
// to the lifecycle announcements, and owes the backup_failed rules a notification for a backup
// that failed, writing the delivery intents in the same transaction that makes the job terminal,
// so a notification is owed exactly when the failure it reports commits.
//
// The rules and destinations are read through the reader pool rather than the caller's
// transaction: the write is what has to be atomic with the job's outcome, not the lookup of who
// to tell.
func (n *Notifier) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	n.announceLifecycle(ctx, fin)
	if fin.Kind != jobs.KindBackup || fin.Status != jobs.StatusFailed {
		return nil
	}
	reason := failureReason(fin.Error)
	n.record(ctx, tx, n.prepareByRules(ctx, alerts.KindBackupFailed, deref(fin.InstanceID),
		func(p *alerts.ParamsWire) *notify.Event {
			return &notify.Event{
				Kind:       notify.KindBackupFailed,
				InstanceID: deref(fin.InstanceID),
				// Named from the job's own submission, so nothing here reads a row inside the
				// finish transaction.
				InstanceName: fin.InstanceName,
				Detail: append(shown(p, []shownField{{alerts.FieldReason, "Reason", reason}}),
					notify.Field{
						Key:  "what_to_do",
						Name: "What to do",
						Value: "Open Operations on the server's overview for the full log, " +
							"then run the backup again.",
					},
					notify.Field{Key: "job_id", Name: "Job ID", Value: fin.ID},
				),
			}
		}))
	return nil
}

// record writes the deliveries of prepared in tx, logging a failure: a notification never
// changes the outcome it reports.
func (n *Notifier) record(ctx context.Context, tx *sql.Tx, prepared []ruleEvent) {
	for _, p := range prepared {
		if err := Record(ctx, tx, p.deliveries); err != nil {
			slog.ErrorContext(ctx, "record notification",
				slog.String("event_kind", p.event.Kind.String()), slog.Any("error", err))
		}
	}
}

// NotifyUnexpectedStop is what the observer calls when it finds a server that left `running`
// with no job holding its lock. An operator's own stop holds that lock, so an expected stop never
// reaches here. It sends to the destinations of the instance_down rules covering the server.
//
// The state write has already committed. A crash in the gap costs this one notification, which
// is the trade for keeping the observer's write path free of a notification's transaction.
func (n *Notifier) NotifyUnexpectedStop(ctx context.Context, inst *store.Instance, to, reason string) {
	now := time.Now().UTC()
	detail := downDetail(to, reason, inst.MemLimitMB)
	n.emitByRules(ctx, alerts.KindInstanceDown, inst.ID, func(p *alerts.ParamsWire) *notify.Event {
		fields := shown(p, []shownField{
			{alerts.FieldStatus, "Status", detail[0].Value},
			{alerts.FieldReason, "Cause", detail[1].Value},
			{alerts.FieldTime, "Time", formatInstant(now, p.Location())},
		})
		return &notify.Event{
			Kind: notify.KindInstanceDown, InstanceID: inst.ID, InstanceName: inst.Name,
			Detail: append(fields, detail[2]),
		}
	})
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

// NotifySignInsBlocked reports to the destinations of the sign_ins_blocked rules that sign-ins to
// the account username are refused after repeated failures, the first refused attempt coming from address.
func (n *Notifier) NotifySignInsBlocked(ctx context.Context, username, address string) {
	n.emitByRules(ctx, alerts.KindSignInsBlocked, "", func(*alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindSignInsBlocked,
			Detail: []notify.Field{
				{Key: "username", Name: "Account", Value: username},
				{Key: "address", Name: "Blocked attempt from", Value: address},
			},
		}
	})
}

// NotifyPublicBuild owes the new_game_build rules a notification when the observed public build
// is one the panel has not seen before. An unchanged observation is the common case, since the
// check runs hourly, and says nothing, so a receiver is told about a new build once rather than
// every hour until someone updates. It returns the finish step that records the deliveries, nil
// when nothing is owed.
func (n *Notifier) NotifyPublicBuild(
	ctx context.Context, previous, observed string,
) func(context.Context, *sql.Tx) error {
	if observed == "" || observed == previous {
		return nil
	}
	prepared := n.prepareByRules(ctx, alerts.KindNewGameBuild, "", func(p *alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: notify.KindUpdateAvailable,
			Detail: append(shown(p, []shownField{{alerts.FieldBuild, "Available build", observed}}),
				notify.Field{
					Key:   "what_to_do",
					Name:  "What to do",
					Value: "Stop each server, then update it from its page in Valmin.",
				}),
		}
	})
	if len(prepared) == 0 {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		n.record(ctx, tx, prepared)
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
// observer saw, and what the reader can do about it. limitMB is the server's memory limit, 0 for
// none.
func downDetail(to, reason string, limitMB int) []notify.Field {
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
	switch reason {
	case instance.ReasonOOMKilled:
		next = memoryAdvice(limitMB)
	case instance.ReasonUnresponsive:
		status = "Not responding; still running"
		next = "Check the server's console log and any mod you added or updated recently. " +
			"Restart the server if it stays stuck."
	case instance.ReasonUnresponsiveRestarting:
		status = "Being restarted"
		next = "Anyone playing was disconnected. A hung game cannot save, so the world may go back " +
			"to its last autosave, up to 30 minutes old."
	}
	return []notify.Field{
		{Key: "status", Name: "Status", Value: status},
		{Key: "reason", Name: "Cause", Value: stopCause(reason, limitMB)},
		{Key: "what_to_do", Name: "What to do", Value: next},
	}
}

// stopCause is the observer's reason for a stop as a sentence, in plain words for a server the
// kernel killed for memory.
func stopCause(reason string, limitMB int) string {
	if reason != instance.ReasonOOMKilled {
		return asSentence(reason)
	}
	if limitMB > 0 {
		return fmt.Sprintf("The server used all %d MB its memory limit allows, so the kernel stopped it. "+
			"The world may be damaged.", limitMB)
	}
	return "The host ran out of memory and the kernel stopped the server. The world may be damaged."
}

// memoryAdvice is what to do about a server the kernel killed for memory.
func memoryAdvice(limitMB int) string {
	if limitMB > 0 {
		return "Raise the memory limit in the server's settings, then open the server in Valmin " +
			"and choose Check this server."
	}
	return "Free memory on the host, for example by stopping a server you are not using, " +
		"then open the server in Valmin and choose Check this server."
}
