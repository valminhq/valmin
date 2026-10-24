package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// The step types a chain may contain.
const (
	StepCommand    = "command"
	StepWait       = "wait"
	StepBackup     = "backup"
	StepStop       = "stop"
	StepStart      = "start"
	StepRestart    = "restart"
	StepGameUpdate = "game_update"
)

// Bounds of a chain.
const (
	MaxChainSteps  = 20
	MaxWaitSeconds = 3600
)

// ChainStep is one step of a chain. Command applies to StepCommand and Seconds to StepWait.
type ChainStep struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

// ChainPayload is what a chain schedule stores and a chain job records.
type ChainPayload struct {
	Steps []ChainStep `json:"steps"`
}

// ParseChainPayload reads the steps out of a chain schedule's stored payload.
func ParseChainPayload(raw string) ([]ChainStep, error) {
	var p ChainPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("decode chain payload: %w", err)
	}
	return p.Steps, nil
}

// The reasons a step list is refused, named as the API's field codes are.
const (
	ProblemRequired   = "required"
	ProblemInvalid    = "invalid"
	ProblemOutOfRange = "out_of_range"
)

// StepProblem is one reason a step list is refused, against the field that carries it.
type StepProblem struct {
	Field   string
	Reason  string
	Message string
}

// ValidateSteps checks a step list and trims each command in place. It returns every problem
// found, so a client can show them all at once.
func ValidateSteps(steps []ChainStep) []StepProblem {
	if len(steps) == 0 {
		return []StepProblem{{"steps", ProblemRequired, "A chain needs at least one step."}}
	}
	if len(steps) > MaxChainSteps {
		return []StepProblem{{
			"steps", ProblemOutOfRange, fmt.Sprintf("A chain has at most %d steps.", MaxChainSteps),
		}}
	}
	var problems []StepProblem
	for i := range steps {
		problems = append(problems, validateStep(i, &steps[i])...)
	}
	return problems
}

func validateStep(i int, s *ChainStep) []StepProblem {
	field := func(name string) string { return fmt.Sprintf("steps[%d].%s", i, name) }
	switch s.Type {
	case StepBackup, StepStop, StepStart, StepRestart, StepGameUpdate:
		return nil
	case StepWait:
		if s.Seconds < 1 || s.Seconds > MaxWaitSeconds {
			return []StepProblem{{
				field("seconds"), ProblemOutOfRange,
				fmt.Sprintf("Between 1 and %d seconds.", MaxWaitSeconds),
			}}
		}
		return nil
	case StepCommand:
		line, err := command.Validate(s.Command, command.ScopeAny)
		if err != nil {
			return []StepProblem{{field("command"), ProblemInvalid, "One line of at most 1,024 bytes."}}
		}
		s.Command = line
		return nil
	default:
		return []StepProblem{{field("type"), ProblemInvalid, "This is not a step a chain can run."}}
	}
}

// StopsServer reports whether any step takes a running server down.
func StopsServer(steps []ChainStep) bool {
	for _, s := range steps {
		switch s.Type {
		case StepBackup, StepStop, StepRestart, StepGameUpdate:
			return true
		}
	}
	return false
}

func (s ChainStep) String() string {
	switch s.Type {
	case StepCommand:
		return "send " + s.Command
	case StepWait:
		return fmt.Sprintf("wait %d seconds", s.Seconds)
	default:
		return s.Type
	}
}

var (
	errChainCancelled = errors.New("the chain was cancelled")
	errWrongState     = errors.New("the server is not in a state this step can run from")
	errCommandFailed  = errors.New("the command was not delivered")
)

// childJobError is a step's job ending without success.
type childJobError struct {
	kind, status, code, message string
}

func (e *childJobError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("the %s job %s", e.kind, e.status)
	}
	return fmt.Sprintf("the %s job %s: %s", e.kind, e.status, e.message)
}

const (
	defaultChainPoll    = 2 * time.Second
	defaultChainSettle  = 10 * time.Minute
	chainCommandTimeout = 30 * time.Second
)

// Chainer runs a schedule's steps against one instance in order, stopping at the first that
// fails. Each lifecycle step is submitted as the job it normally is and waited for.
type Chainer struct {
	DB          *store.DB
	Engine      *jobs.Engine
	Commands    commandSender
	Backupper   *Backupper
	Stopper     *Stopper
	Starter     *Starter
	Restarter   *Restarter
	GameUpdater *GameUpdater
	// Poll is how often a waiting step checks its job and the chain's cancel flag. Zero means
	// two seconds.
	Poll time.Duration
	// Settle is how long a backup step waits for the server it stopped to be started again.
	// Zero means ten minutes.
	Settle time.Duration
}

// ChainSubmission carries the inputs recorded when a chain job is queued.
type ChainSubmission struct {
	Instance   *store.Instance
	ScheduleID string
	Steps      []ChainStep
}

// Submit queues the chain. A chain with a game update step is refused for a modded server,
// which an update needs a person to confirm.
func (c *Chainer) Submit(ctx context.Context, in *ChainSubmission) (*store.Job, error) {
	for _, s := range in.Steps {
		if s.Type != StepGameUpdate {
			continue
		}
		if err := ConfirmModded(in.Instance, false); err != nil {
			return nil, err
		}
		break
	}
	id := in.Instance.ID
	job, err := c.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindChain, LockKey: jobs.ChainLockKey(id),
		InstanceID: &id, InstanceName: in.Instance.Name, ScheduleID: in.ScheduleID,
		Payload: ChainPayload{Steps: in.Steps},
	}, c.Run(id, in.ScheduleID, in.Steps))
	if err != nil {
		return nil, fmt.Errorf("submit chain for instance %s: %w", id, err)
	}
	return job, nil
}

// ChainCancelPolicy lets a chain be cancelled at any point: the step in flight finishes on its
// own and no further step runs.
func ChainCancelPolicy(string) (cancellable bool, phase string) { return true, "" }

// Run is the chain job.
func (c *Chainer) Run(instanceID, scheduleID string, steps []ChainStep) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		for i, step := range steps {
			if jh.CancelRequested(ctx) {
				return jobs.Outcome{Status: jobs.StatusCancelled}
			}
			jh.Progress(ctx, i*100/len(steps), fmt.Sprintf("step %d of %d: %s", i+1, len(steps), step))
			err := c.runStep(ctx, jh, instanceID, scheduleID, step)
			switch {
			case err == nil:
				jh.Log(fmt.Sprintf("step %d done", i+1))
			case errors.Is(err, errChainCancelled):
				return jobs.Outcome{Status: jobs.StatusCancelled}
			case ctx.Err() != nil, errors.Is(err, jobs.ErrShuttingDown):
				return jobs.Failed(errcode.Interrupted, fmt.Errorf("step %d (%s): %w", i+1, step, err))
			default:
				return jobs.Outcome{
					Status: jobs.StatusFailed, ErrorCode: chainFailureCode(err),
					Error: fmt.Sprintf("step %d of %d (%s) failed: %v", i+1, len(steps), step, err),
				}
			}
		}
		jh.Progress(ctx, 100, "all steps done")
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

// chainFailureCode names why a step failed in the registry's terms: the code of the job that
// failed when there is one.
func chainFailureCode(err error) string {
	var child *childJobError
	var conflict *store.JobConflict
	switch {
	case errors.As(err, &child) && child.code != "":
		return child.code
	case errors.As(err, &conflict):
		return errcode.JobInProgress.String()
	case errors.Is(err, errWrongState):
		return errcode.InvalidState.String()
	case errors.Is(err, errCommandFailed):
		return errcode.Unavailable.String()
	default:
		return errcode.Internal.String()
	}
}

func (c *Chainer) runStep(ctx context.Context, jh *jobs.Handle, id, scheduleID string, step ChainStep) error {
	switch step.Type {
	case StepWait:
		return c.pause(ctx, jh, time.Duration(step.Seconds)*time.Second)
	case StepCommand:
		return c.sendCommand(ctx, jh, id, step.Command)
	}
	inst, err := c.fresh(ctx, id)
	if err != nil {
		return err
	}
	switch step.Type {
	case StepBackup:
		return c.backup(ctx, jh, inst, scheduleID)
	case StepStop:
		return c.stop(ctx, jh, inst, scheduleID)
	case StepStart:
		return c.start(ctx, jh, inst, scheduleID)
	case StepRestart:
		return c.restart(ctx, jh, inst, scheduleID)
	case StepGameUpdate:
		return c.gameUpdate(ctx, jh, inst, scheduleID)
	default:
		return fmt.Errorf("no runner for chain step %q", step.Type)
	}
}

// backup takes a quiesced backup. A server it stopped is waited for until it is running again.
func (c *Chainer) backup(ctx context.Context, jh *jobs.Handle, inst *store.Instance, scheduleID string) error {
	wasRunning := instance.State(inst.State) == instance.StateRunning
	err := c.submitAndWait(ctx, jh, func() (*store.Job, error) {
		return c.Backupper.Submit(ctx, &BackupSubmission{
			Instance: inst, ContainerID: deref(inst.ContainerID), Mode: BackupQuiesced, ScheduleID: scheduleID,
		})
	})
	if err != nil || !wasRunning {
		return err
	}
	return c.awaitState(ctx, jh, inst.ID, instance.StateRunning)
}

// stop stops a running server. A stopped one satisfies the step.
func (c *Chainer) stop(ctx context.Context, jh *jobs.Handle, inst *store.Instance, scheduleID string) error {
	state := instance.State(inst.State)
	if state == instance.StateStopped {
		jh.Log("the server is already stopped")
		return nil
	}
	if state != instance.StateRunning {
		return fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	return c.submitAndWait(ctx, jh, func() (*store.Job, error) {
		return c.Stopper.Submit(ctx, &StopSubmission{
			Instance: inst, ContainerID: deref(inst.ContainerID), ScheduleID: scheduleID,
		})
	})
}

// start starts a stopped server. A running one satisfies the step.
func (c *Chainer) start(ctx context.Context, jh *jobs.Handle, inst *store.Instance, scheduleID string) error {
	state, containerID := instance.State(inst.State), deref(inst.ContainerID)
	if state == instance.StateRunning {
		jh.Log("the server is already running")
		return nil
	}
	if state != instance.StateStopped || containerID == "" {
		return fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	return c.submitAndWait(ctx, jh, func() (*store.Job, error) {
		return c.Starter.Submit(ctx, &StartSubmission{Instance: inst, ContainerID: containerID, ScheduleID: scheduleID})
	})
}

// restart restarts a running server.
func (c *Chainer) restart(ctx context.Context, jh *jobs.Handle, inst *store.Instance, scheduleID string) error {
	state, containerID := instance.State(inst.State), deref(inst.ContainerID)
	if state != instance.StateRunning || containerID == "" {
		return fmt.Errorf("%w: it is %s", errWrongState, state)
	}
	return c.submitAndWait(ctx, jh, func() (*store.Job, error) {
		return c.Restarter.Submit(
			ctx,
			&RestartSubmission{Instance: inst, ContainerID: containerID, ScheduleID: scheduleID},
		)
	})
}

// gameUpdate updates the game on a stopped server.
func (c *Chainer) gameUpdate(ctx context.Context, jh *jobs.Handle, inst *store.Instance, scheduleID string) error {
	if state := instance.State(inst.State); state != instance.StateStopped {
		return fmt.Errorf("%w: a game update needs a stopped server, and it is %s", errWrongState, state)
	}
	return c.submitAndWait(ctx, jh, func() (*store.Job, error) {
		return c.GameUpdater.Submit(ctx, &GameUpdateSubmission{Instance: inst, ScheduleID: scheduleID})
	})
}

// sendCommand sends one console command. The step is skipped when the server is not running
// or has no command channel.
func (c *Chainer) sendCommand(ctx context.Context, jh *jobs.Handle, id, line string) error {
	inst, err := c.fresh(ctx, id)
	if err != nil {
		return err
	}
	if instance.State(inst.State) != instance.StateRunning {
		jh.Log("skipped: the server is not running")
		return nil
	}
	if c.Commands == nil {
		jh.Log("skipped: no command channel is configured")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, chainCommandTimeout)
	defer cancel()
	reply, err := c.Commands.Send(ctx, inst, line)
	switch {
	case err == nil:
		jh.Log("sent: " + line)
		if reply = strings.TrimSpace(reply); reply != "" {
			jh.Log(reply)
		}
		return nil
	case errors.Is(err, command.ErrUnsupported):
		jh.Log("skipped: the server has no RCON mod")
		return nil
	case errors.Is(err, command.ErrInvalidState):
		jh.Log("skipped: the server is not running")
		return nil
	default:
		return fmt.Errorf("%w: %w", errCommandFailed, err)
	}
}

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

// submitAndWait submits a step's job and waits for it to end.
func (c *Chainer) submitAndWait(
	ctx context.Context, jh *jobs.Handle, submit func() (*store.Job, error),
) error {
	job, err := submit()
	if err != nil {
		return err
	}
	for {
		row, err := c.Engine.Get(ctx, job.ID)
		if err != nil {
			return fmt.Errorf("read job %s: %w", job.ID, err)
		}
		if row == nil {
			return fmt.Errorf("job %s is gone", job.ID)
		}
		switch row.Status {
		case jobs.StatusSucceeded:
			return nil
		case jobs.StatusFailed, jobs.StatusCancelled:
			return &childJobError{
				kind: row.Kind, status: row.Status, code: deref(row.ErrorCode), message: deref(row.Error),
			}
		}
		if err := c.sleep(ctx, jh, c.poll()); err != nil {
			return err
		}
	}
}

// awaitState waits for the instance to reach want, and fails once it is in error or the settle
// time has passed.
func (c *Chainer) awaitState(ctx context.Context, jh *jobs.Handle, id string, want instance.State) error {
	settle := c.Settle
	if settle <= 0 {
		settle = defaultChainSettle
	}
	deadline := time.Now().Add(settle)
	for {
		inst, err := c.fresh(ctx, id)
		if err != nil {
			return err
		}
		switch state := instance.State(inst.State); {
		case state == want:
			return nil
		case state == instance.StateError || time.Now().After(deadline):
			return fmt.Errorf("%w: waiting for %s, it is %s", errWrongState, want, state)
		}
		if err := c.sleep(ctx, jh, c.poll()); err != nil {
			return err
		}
	}
}

// pause waits d, noticing a cancel request within one poll.
func (c *Chainer) pause(ctx context.Context, jh *jobs.Handle, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		if err := c.sleep(ctx, jh, min(left, c.poll())); err != nil {
			return err
		}
	}
}

// sleep waits d, then reports a cancel request as errChainCancelled.
func (c *Chainer) sleep(ctx context.Context, jh *jobs.Handle, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait: %w", ctx.Err())
	case <-t.C:
	}
	if jh.CancelRequested(ctx) {
		return errChainCancelled
	}
	return nil
}

func (c *Chainer) poll() time.Duration {
	if c.Poll > 0 {
		return c.Poll
	}
	return defaultChainPoll
}
