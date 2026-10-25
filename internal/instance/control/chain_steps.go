package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// StepOrigin records who asked for a step's job: the schedule that fired it, or the person who
// requested it.
type StepOrigin struct {
	ScheduleID  string
	RequestedBy string
}

// errSatisfied is returned by SubmitStep when the instance is already where the step leads, so
// there is no job to run.
var errSatisfied = errors.New("nothing to do")

// SubmitStep submits the job a lifecycle step (backup, stop, start, restart or game update)
// runs as. It returns errSatisfied when the instance is already where the step leads, and
// errWrongState when the instance cannot take the step from its state.
func (c *Chainer) SubmitStep(
	ctx context.Context, inst *store.Instance, step *ChainStep, origin StepOrigin,
) (*store.Job, error) {
	switch step.Type {
	case StepBackup:
		return c.Backupper.Submit(ctx, &BackupSubmission{
			Instance: inst, ContainerID: deref(inst.ContainerID), Mode: BackupQuiesced,
			RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID, Trigger: step.Trigger,
		})
	case StepStop:
		return c.submitStop(ctx, inst, origin)
	case StepStart:
		return c.submitStart(ctx, inst, origin)
	case StepRestart:
		return c.submitRestart(ctx, inst, origin)
	case StepGameUpdate:
		return c.submitGameUpdate(ctx, inst, origin)
	default:
		return nil, fmt.Errorf("no job for chain step %q", step.Type)
	}
}

// submitStop stops a running server. A stopped one satisfies the step.
func (c *Chainer) submitStop(ctx context.Context, inst *store.Instance, origin StepOrigin) (*store.Job, error) {
	state := instance.State(inst.State)
	if state == instance.StateStopped {
		return nil, fmt.Errorf("%w: the server is already stopped", errSatisfied)
	}
	if state != instance.StateRunning {
		return nil, fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	return c.Stopper.Submit(ctx, &StopSubmission{
		Instance: inst, ContainerID: deref(inst.ContainerID),
		RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID,
	})
}

// submitStart starts a stopped server. A running one satisfies the step.
func (c *Chainer) submitStart(ctx context.Context, inst *store.Instance, origin StepOrigin) (*store.Job, error) {
	state, containerID := instance.State(inst.State), deref(inst.ContainerID)
	if state == instance.StateRunning {
		return nil, fmt.Errorf("%w: the server is already running", errSatisfied)
	}
	if state != instance.StateStopped || containerID == "" {
		return nil, fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	return c.Starter.Submit(ctx, &StartSubmission{
		Instance: inst, ContainerID: containerID,
		RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID,
	})
}

// submitRestart restarts a running server.
func (c *Chainer) submitRestart(ctx context.Context, inst *store.Instance, origin StepOrigin) (*store.Job, error) {
	state, containerID := instance.State(inst.State), deref(inst.ContainerID)
	if state != instance.StateRunning {
		return nil, fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	if containerID == "" {
		return nil, errors.New("the instance has no container")
	}
	return c.Restarter.Submit(ctx, &RestartSubmission{
		Instance: inst, ContainerID: containerID,
		RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID,
	})
}

// submitGameUpdate updates the game on a stopped server.
func (c *Chainer) submitGameUpdate(ctx context.Context, inst *store.Instance, origin StepOrigin) (*store.Job, error) {
	if state := instance.State(inst.State); state != instance.StateStopped {
		return nil, fmt.Errorf("%w: a game update needs a stopped server, and it is %s", errWrongState, state)
	}
	return c.GameUpdater.Submit(ctx, &GameUpdateSubmission{
		Instance: inst, RequestedBy: origin.RequestedBy, ScheduleID: origin.ScheduleID,
	})
}

// runStep carries out one step of the chain in against its instance.
func (c *Chainer) runStep(ctx context.Context, jh *jobs.Handle, in *ChainSubmission, step *ChainStep) error {
	id := in.Instance.ID
	origin := StepOrigin{ScheduleID: in.ScheduleID, RequestedBy: in.RequestedBy}
	switch step.Type {
	case StepWait:
		if err := jh.Sleep(ctx, time.Duration(step.Seconds)*time.Second); err != nil {
			return fmt.Errorf("wait %d seconds: %w", step.Seconds, err)
		}
		return nil
	case StepCommand:
		return c.sendCommand(ctx, jh, id, step)
	case StepWaitFor:
		return c.waitFor(ctx, jh, id, step)
	}
	inst, err := c.fresh(ctx, id)
	if err != nil {
		return err
	}
	job, err := c.SubmitStep(ctx, inst, step, origin)
	if errors.Is(err, errSatisfied) {
		jh.Log(strings.TrimPrefix(err.Error(), errSatisfied.Error()+": "))
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.waitChild(ctx, jh, job); err != nil {
		return err
	}
	if step.Type == StepBackup && instance.State(inst.State) == instance.StateRunning {
		return c.awaitState(ctx, jh, inst.ID, instance.StateRunning)
	}
	return nil
}

// sendCommand sends one console command, retrying while the server's RCON listener comes up.
// The step is skipped when the server is not running or has no command channel, unless it must
// be sent, in which case each of those fails the step.
func (c *Chainer) sendCommand(ctx context.Context, jh *jobs.Handle, id string, step *ChainStep) error {
	inst, err := c.fresh(ctx, id)
	if err != nil {
		return err
	}
	if instance.State(inst.State) != instance.StateRunning {
		return skipOrFail(jh, step, "the server is not running")
	}
	if c.Commands == nil {
		return skipOrFail(jh, step, "no command channel is configured")
	}
	sendCtx, cancel := context.WithTimeout(ctx, chainCommandTimeout+defaultSendWindow)
	defer cancel()
	reply, err := c.sendWhenReady(sendCtx, jh, inst, step.Command)
	switch {
	case err == nil:
		jh.Log("sent: " + step.Command)
		if reply = strings.TrimSpace(reply); reply != "" {
			jh.Log(reply)
		}
		return nil
	case errors.Is(err, jobs.ErrCancelRequested), ctx.Err() != nil:
		return err
	case errors.Is(err, command.ErrUnsupported):
		return skipOrFail(jh, step, "the server has no RCON mod")
	case errors.Is(err, command.ErrInvalidState):
		return skipOrFail(jh, step, "the server is not running")
	default:
		return fmt.Errorf("%w: %w", errCommandFailed, err)
	}
}

// skipOrFail logs why a command step did nothing, or fails the step when it must be sent.
func skipOrFail(jh *jobs.Handle, step *ChainStep, reason string) error {
	if step.MustSend {
		return fmt.Errorf("%w: %s", errCommandFailed, reason)
	}
	jh.Log("skipped: " + reason)
	return nil
}

// fresh reads the instance row again, since a step may follow one that changed it.
func (c *Chainer) fresh(ctx context.Context, id string) (*store.Instance, error) {
	inst, err := c.DB.InstanceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read instance %s: %w", id, err)
	}
	if inst == nil {
		return nil, fmt.Errorf("instance %s is gone", id)
	}
	return inst, nil
}

// defaultSendWindow is how long a command keeps being retried while the server's RCON listener
// comes up.
const defaultSendWindow = 30 * time.Second

// sendRetryInterval is the pause between attempts of a retried send.
const sendRetryInterval = 2 * time.Second

// sendWhenReady delivers line to a running server, retrying until the send window closes while the
// listener is not up yet. A send that reached the server, a refused command and a missing or
// stopped server are not retried. A cancel of jh ends the wait.
func (c *Chainer) sendWhenReady(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, line string,
) (string, error) {
	deadline := time.Now().Add(defaultSendWindow)
	for {
		reply, err := c.Commands.Send(ctx, inst, line)
		if err == nil {
			return reply, nil
		}
		if !retryableSend(err) || time.Now().After(deadline) {
			return "", fmt.Errorf("send %q: %w", line, err)
		}
		if err := jh.Sleep(ctx, sendRetryInterval); err != nil {
			return "", fmt.Errorf("send %q: %w", line, err)
		}
	}
}

// retryableSend reports whether a failed send may succeed if tried again shortly. A send that
// failed after the command reached the server is not retried: it may already be running.
func retryableSend(err error) bool {
	return !errors.Is(err, command.ErrUnsupported) && !errors.Is(err, command.ErrInvalidCommand) &&
		!errors.Is(err, command.ErrCommandForbidden) && !errors.Is(err, command.ErrNoReply) &&
		!errors.Is(err, command.ErrInvalidState)
}

// waitFor holds the chain until a condition of the running server holds or the step's limit
// passes. A limit that passes is a warning and the chain goes on. The step is skipped when the
// server is not running, since it can reach neither condition.
func (c *Chainer) waitFor(ctx context.Context, jh *jobs.Handle, id string, step *ChainStep) error {
	inst, err := c.fresh(ctx, id)
	if err != nil {
		return err
	}
	if instance.State(inst.State) != instance.StateRunning || inst.ContainerID == nil {
		jh.Log("skipped: the server is not running")
		return nil
	}
	limit := time.Duration(step.Seconds) * time.Second
	waitCtx, release := cancelOnRequest(ctx, jh)
	defer release()
	switch step.Condition {
	case ConditionReady:
		err = c.waitReady(waitCtx, jh, *inst.ContainerID, limit)
	case ConditionEmpty:
		err = c.waitEmpty(waitCtx, jh, inst, limit)
	default:
		return fmt.Errorf("no condition %q to wait for", step.Condition)
	}
	if err != nil && ctx.Err() == nil && waitCtx.Err() != nil {
		return jobs.ErrCancelRequested
	}
	return err
}

// waitReady waits for the container's current boot to log that it is ready.
func (c *Chainer) waitReady(ctx context.Context, jh *jobs.Handle, containerID string, limit time.Duration) error {
	ready, err := c.Streams.AwaitReady(ctx, containerID, limit)
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("wait for the server to be ready: %w", ctx.Err())
	case err != nil:
		return fmt.Errorf("%w: %w", errWrongState, err)
	case ready:
		jh.Log("the server is ready")
	default:
		jh.Log(fmt.Sprintf("warning: the server did not report ready within %s; continuing", limit))
	}
	return nil
}

// waitEmpty waits for the server's own player count to read zero. An unknown count keeps
// waiting. It fails if the server stops while it waits.
func (c *Chainer) waitEmpty(ctx context.Context, jh *jobs.Handle, inst *store.Instance, limit time.Duration) error {
	reader := c.Streams.Open(inst.ID, *inst.ContainerID) //nolint:contextcheck // the reader outlives this wait
	counted, stopWatch := reader.WatchPlayers()
	defer stopWatch()
	stateChanged, stopStates := c.states.subscribe(inst.ID)
	defer stopStates()
	recheck := time.NewTicker(stateRecheck)
	defer recheck.Stop()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		if err := c.requireState(ctx, inst.ID, instance.StateRunning); err != nil {
			return fmt.Errorf("wait for no players: %w", err)
		}
		if players := reader.Players(); players != nil && *players == 0 {
			jh.Log("no players are connected")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for no players: %w", ctx.Err())
		case <-timer.C:
			jh.Log(fmt.Sprintf("warning: players were still connected after %s; continuing", limit))
			return nil
		case <-stateChanged:
		case <-recheck.C:
		case <-counted:
		}
	}
}
