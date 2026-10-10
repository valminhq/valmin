package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// The packages that provide the world tools, as instance_mods names them.
const (
	UpgradeWorldPackage = "JereKuusela-Upgrade_World"
	FreshWorldPackage   = "sighsorry-FreshWorld"
)

// The world tools and their actions, as the API names them.
const (
	ToolUpgradeWorld = "upgrade_world"
	ToolFreshWorld   = "fresh_world"

	ActionZonesReset = "zones_reset"
	ActionUpgrade    = "upgrade"
	ActionWorldClean = "world_clean"
	ActionRun        = "run"
)

// maxMinDistanceM bounds a zones reset's minimum distance from the world centre.
const maxMinDistanceM = 20000

// ErrInvalidWorldTool is a world tool request that names no known action or carries a bad
// argument.
var ErrInvalidWorldTool = errors.New("invalid world tool request")

var upgradeOperation = regexp.MustCompile(`^[a-z][a-z_]*$`)

// WorldTool is one world-maintenance action that a mod runs inside the server.
type WorldTool struct {
	Tool         string `json:"tool"`
	Action       string `json:"action"`
	Operation    string `json:"operation,omitempty"`
	MinDistanceM int    `json:"min_distance_m,omitempty"`
}

// Package is the package that must be installed for the tool to run, or "" for an unknown tool.
func (t WorldTool) Package() string {
	switch t.Tool {
	case ToolUpgradeWorld:
		return UpgradeWorldPackage
	case ToolFreshWorld:
		return FreshWorldPackage
	}
	return ""
}

// Command builds the RCON line that runs the action through the server console. Upgrade World
// commands carry `start` so they execute instead of only listing the zones they would touch.
func (t WorldTool) Command() (string, error) {
	if t.Operation != "" && t.Action != ActionUpgrade {
		return "", fmt.Errorf("%w: operation applies only to upgrade", ErrInvalidWorldTool)
	}
	if t.MinDistanceM != 0 && t.Action != ActionZonesReset {
		return "", fmt.Errorf("%w: min_distance_m applies only to zones_reset", ErrInvalidWorldTool)
	}
	build, ok := worldToolLines[[2]string{t.Tool, t.Action}]
	if !ok {
		return "", fmt.Errorf("%w: unknown tool or action %q/%q", ErrInvalidWorldTool, t.Tool, t.Action)
	}
	line, err := build(t)
	if err != nil {
		return "", err
	}
	return "consoleCommand " + line, nil
}

// worldToolLines builds each tool action's console line, keyed by tool and action.
var worldToolLines = map[[2]string]func(WorldTool) (string, error){
	{ToolUpgradeWorld, ActionZonesReset}: zonesResetLine,
	{ToolUpgradeWorld, ActionUpgrade}:    upgradeLine,
	{ToolUpgradeWorld, ActionWorldClean}: fixedLine("world_clean start"),
	{ToolFreshWorld, ActionRun}:          fixedLine("freshworld"),
}

func zonesResetLine(t WorldTool) (string, error) {
	if t.MinDistanceM < 0 || t.MinDistanceM > maxMinDistanceM {
		return "", fmt.Errorf("%w: min_distance_m must be 0 to %d", ErrInvalidWorldTool, maxMinDistanceM)
	}
	if t.MinDistanceM > 0 {
		return fmt.Sprintf("zones_reset start min=%d", t.MinDistanceM), nil
	}
	return "zones_reset start", nil
}

// upgradeLine refuses operations ending in `_worldgen`: they move rivers and destroy bases.
func upgradeLine(t WorldTool) (string, error) {
	if !upgradeOperation.MatchString(t.Operation) || strings.HasSuffix(t.Operation, "_worldgen") {
		return "", fmt.Errorf("%w: operation %q is not allowed", ErrInvalidWorldTool, t.Operation)
	}
	return "upgrade " + t.Operation + " start", nil
}

func fixedLine(line string) func(WorldTool) (string, error) {
	return func(WorldTool) (string, error) { return line, nil }
}

// commandSender sends one console command to a running server.
type commandSender interface {
	Send(ctx context.Context, inst *store.Instance, raw string, unrestricted bool) (string, error)
}

// defaultSendWindow is how long a world tool keeps retrying its command after the server is
// ready, while the RCON listener comes up.
const defaultSendWindow = 30 * time.Second

// WorldTooler runs a world tool on a stopped server: it backs up the world, starts the server
// and sends the tool's command over RCON. The server is left running.
type WorldTooler struct {
	DB          *store.DB
	Engine      *jobs.Engine
	Snapshotter *Snapshotter
	Starter     *Starter
	Commands    commandSender
	SendWindow  time.Duration
}

// WorldToolSubmission carries the inputs recorded when a world tool job is queued.
type WorldToolSubmission struct {
	Instance    *store.Instance
	ContainerID string
	Tool        WorldTool
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit claims stopped to starting and queues the world tool.
func (w *WorldTooler) Submit(ctx context.Context, input *WorldToolSubmission) (*store.Job, error) {
	line, err := input.Tool.Command()
	if err != nil {
		return nil, err
	}
	id := input.Instance.ID
	job, err := w.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindWorldTool, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name, RequestedBy: input.RequestedBy,
		Payload: input.Tool, Audit: input.Audit,
		OnClaim: transitionClaim(jobs.KindWorldTool, id, instance.StateStopped, instance.StateStarting),
	}, w.Run(input.Instance, input.ContainerID, line))
	if err != nil {
		return nil, fmt.Errorf("submit world tool for instance %s: %w", id, err)
	}
	return job, nil
}

// Run is the world tool job. A failed backup returns the instance to stopped with nothing
// changed; once the server is up, the backup is recorded and the instance ends running whether
// or not the command was sent.
func (w *WorldTooler) Run(inst *store.Instance, containerID, line string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "backing up the world")
		snapshot, err := w.Snapshotter.Snapshot(ctx, inst, store.TriggerPreUpdate)
		if err != nil {
			return jobs.Outcome{
				Status: jobs.StatusFailed, ErrorCode: failureCode(err).String(),
				Error:    fmt.Sprintf("could not back up the world: %v", err),
				OnFinish: finishToStopped(inst.ID),
			}
		}

		jh.Progress(ctx, 20, "starting container")
		out := w.Starter.startAndAwaitReady(ctx, jh, inst.ID, containerID)
		out.OnFinish = chainFinish(snapshot, out.OnFinish)
		if out.Status != jobs.StatusSucceeded {
			return out
		}

		jh.Log("sending: " + line)
		reply, err := w.send(ctx, inst.ID, line)
		if err != nil {
			out.Status, out.ErrorCode = jobs.StatusFailed, errcode.Unavailable.String()
			out.Error = fmt.Sprintf("the server is running, but the command was not sent: %v", err)
			return out
		}
		jh.Log(reply)
		jh.Progress(ctx, 100, "sent "+strings.TrimPrefix(line, "consoleCommand "))
		return out
	}
}

// send delivers line to the server that has just become ready, retrying until the send
// window closes. The row is read again for the container a start may have rebuilt, and is
// presented as running because the state flip lands only when this job finishes.
func (w *WorldTooler) send(ctx context.Context, instanceID, line string) (string, error) {
	if w.Commands == nil {
		return "", errors.New("no command channel is configured")
	}
	inst, err := w.DB.InstanceByID(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("read instance %s: %w", instanceID, err)
	}
	if inst == nil {
		return "", fmt.Errorf("instance %s is gone", instanceID)
	}
	running := *inst
	running.State = string(instance.StateRunning)

	window := w.SendWindow
	if window <= 0 {
		window = defaultSendWindow
	}
	deadline := time.Now().Add(window)
	for {
		reply, err := w.Commands.Send(ctx, &running, line, true)
		if err == nil {
			return reply, nil
		}
		if !retryableSend(err) || time.Now().After(deadline) {
			return "", fmt.Errorf("send %q: %w", line, err)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("send %q: %w", line, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// retryableSend reports whether a failed send may succeed if tried again shortly.
func retryableSend(err error) bool {
	return !errors.Is(err, command.ErrUnsupported) && !errors.Is(err, command.ErrInvalidCommand) &&
		!errors.Is(err, command.ErrCommandForbidden)
}

// finishToStopped returns an instance claimed into starting to stopped, for a job that failed
// before it started anything.
func finishToStopped(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := instance.SetStateTx(
			ctx,
			tx,
			instanceID,
			instance.StateStarting,
			instance.StateStopped,
		); err != nil {
			return fmt.Errorf("return instance %s to stopped: %w", instanceID, err)
		}
		return nil
	}
}
