package control

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

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
	Send(ctx context.Context, inst *store.Instance, raw string) (string, error)
}

// WorldTooler runs a world tool as a one-time chain: it backs up the stopped world, starts the
// server, waits for the world to load and sends the tool's command over RCON. The server is left
// running.
type WorldTooler struct {
	Chainer *Chainer
	// ReadyTimeout is the longest the chain waits for the world to load before it sends.
	ReadyTimeout time.Duration
}

// WorldToolSubmission carries the inputs recorded when a world tool job is queued.
type WorldToolSubmission struct {
	Instance    *store.Instance
	Tool        WorldTool
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit queues the world tool's chain.
func (w *WorldTooler) Submit(ctx context.Context, input *WorldToolSubmission) (*store.Job, error) {
	line, err := input.Tool.Command()
	if err != nil {
		return nil, err
	}
	snapshot, err := hasWorlds(input.Instance)
	if err != nil {
		return nil, err
	}
	job, err := w.Chainer.Submit(ctx, &ChainSubmission{
		Instance: input.Instance, Kind: jobs.KindWorldTool, From: instance.StateStopped,
		Steps:       worldToolSteps(line, w.ReadyTimeout, snapshot),
		RequestedBy: input.RequestedBy, Audit: input.Audit, Payload: input.Tool,
	})
	if err != nil {
		return nil, fmt.Errorf("submit world tool for instance %s: %w", input.Instance.ID, err)
	}
	return job, nil
}

// hasWorlds reports whether the instance has a worlds directory to archive.
func hasWorlds(inst *store.Instance) (bool, error) {
	_, err := os.Stat(instance.WorldsDir(inst.DataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look for the worlds of instance %s: %w", inst.ID, err)
	}
	return true, nil
}

// worldToolSteps is the chain a world tool runs: a snapshot of the world when there is one, a
// start, a wait for the world to load, then the tool's console line. The line must be delivered,
// so its step is strict.
func worldToolSteps(line string, ready time.Duration, snapshot bool) []ChainStep {
	wait := min(max(int(ready/time.Second), 1), MaxWaitSeconds)
	steps := []ChainStep{
		{Type: StepStart},
		{Type: StepWaitFor, Condition: ConditionReady, Seconds: wait},
		{Type: StepCommand, Command: line, MustSend: true},
	}
	if snapshot {
		steps = append([]ChainStep{{Type: StepBackup, Trigger: store.TriggerPreUpdate}}, steps...)
	}
	return steps
}
