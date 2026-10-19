package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

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
	instanceID := *fin.InstanceID
	switch fin.Kind {
	case jobs.KindStart, jobs.KindRestart:
		n.background(ctx, func(ctx context.Context) { n.NotifyServerStarted(ctx, instanceID, joinCodeWait) })
	case jobs.KindStop:
		payload, _ := fin.Payload.(instance.StopPayload)
		n.background(ctx, func(ctx context.Context) {
			n.NotifyServerStopped(ctx, instanceID, payload.Reason, fin.RequestedBy, lifecycleWait)
		})
	}
}

// NotifyServerStarted reports a server that reached running to the server_started rules
// covering it. Nobody can join before the server logs that its world loaded, nor a crossplay
// server before it logs its join code, so the message waits up to wait for both and is sent
// without them only after that. A server that stops while waited on is not reported.
func (n *Notifier) NotifyServerStarted(ctx context.Context, instanceID string, wait time.Duration) {
	rules := n.rulesFor(ctx, alerts.KindServerStarted, instanceID)
	if len(rules) == 0 {
		return
	}
	var code string
	inst := n.awaitState(ctx, instanceID, instance.StateRunning, wait, func(inst *store.Instance) bool {
		if n.Ready != nil && !n.Ready(ctx, inst) {
			return false
		}
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
	n.emitByRules(ctx, alerts.KindServerStarted, instanceID, func(p *alerts.ParamsWire) *notify.Event {
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
	n.emitByRules(ctx, alerts.KindServerStopped, instanceID, func(p *alerts.ParamsWire) *notify.Event {
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
