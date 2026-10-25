package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	StepWaitFor    = "wait_for"
	StepBackup     = "backup"
	StepStop       = "stop"
	StepStart      = "start"
	StepRestart    = "restart"
	StepGameUpdate = "game_update"
)

// The conditions a wait_for step can wait on.
const (
	ConditionReady = "ready"
	ConditionEmpty = "empty"
)

// Bounds of a chain.
const (
	MaxChainSteps  = 20
	MaxWaitSeconds = 3600
)

// ChainStep is one step of a chain. Command applies to StepCommand. Seconds applies to StepWait
// and, as the longest wait, to StepWaitFor, which also takes a Condition.
type ChainStep struct {
	Type      string `json:"type"`
	Command   string `json:"command,omitempty"`
	Seconds   int    `json:"seconds,omitempty"`
	Condition string `json:"condition,omitempty"`
	// MustSend makes a command step fail instead of being skipped. Set only by code that builds
	// a chain itself, never read from a request.
	MustSend bool `json:"-"`
	// Trigger names the backup trigger of a backup step. Set only by code that builds a chain
	// itself, never read from a request.
	Trigger string `json:"-"`
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
		return secondsProblem(field("seconds"), s.Seconds)
	case StepWaitFor:
		var problems []StepProblem
		if s.Condition != ConditionReady && s.Condition != ConditionEmpty {
			problems = append(problems, StepProblem{
				field("condition"), ProblemInvalid, "Wait for ready or empty.",
			})
		}
		return append(problems, secondsProblem(field("seconds"), s.Seconds)...)
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

// secondsProblem names the field when a wait of seconds is outside the allowed range.
func secondsProblem(field string, seconds int) []StepProblem {
	if seconds >= 1 && seconds <= MaxWaitSeconds {
		return nil
	}
	return []StepProblem{{field, ProblemOutOfRange, fmt.Sprintf("Between 1 and %d seconds.", MaxWaitSeconds)}}
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
	case StepWaitFor:
		if s.Condition == ConditionEmpty {
			return fmt.Sprintf("wait up to %d seconds for no players", s.Seconds)
		}
		return fmt.Sprintf("wait up to %d seconds for the server to be ready", s.Seconds)
	default:
		return s.Type
	}
}

var (
	errWrongState    = errors.New("the server is not in a state this step can run from")
	errCommandFailed = errors.New("the command was not delivered")
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
	defaultChainSettle  = 10 * time.Minute
	chainCommandTimeout = 30 * time.Second
)

// Chainer runs a list of steps against one instance in order, stopping at the first that
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
	// states wakes a step waiting for the instance to reach a state.
	states *stateWatch
	// Streams reads a server's log for the conditions a wait_for step waits on.
	Streams *instance.Streams
	// Settle is how long a backup step waits for the server it stopped to be started again.
	// Zero means ten minutes.
	Settle time.Duration
}

// ChainSubmission carries the inputs recorded when a chain job is queued.
type ChainSubmission struct {
	Instance *store.Instance
	Steps    []ChainStep
	// Kind is the job kind the chain runs as: KindChain, or the kind of a request such as a
	// world tool.
	Kind jobs.Kind
	// ScheduleID or RequestedBy says what triggered the chain; each step's job carries them.
	ScheduleID  string
	RequestedBy string
	Audit       *store.AuditEntry
	// Payload is what the job records.
	Payload any
	// From is the state the instance must be in when the chain begins. Empty accepts any.
	From instance.State
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
		Kind: in.Kind, LockKey: jobs.ChainLockKey(id),
		InstanceID: &id, InstanceName: in.Instance.Name, ScheduleID: in.ScheduleID,
		RequestedBy: in.RequestedBy, Payload: in.Payload, Audit: in.Audit,
	}, c.Run(in))
	if err != nil {
		return nil, fmt.Errorf("submit chain for instance %s: %w", id, err)
	}
	return job, nil
}

// ChainCancelPolicy lets a chain be cancelled at any point: the step in flight finishes on its
// own and no further step runs.
func ChainCancelPolicy(string) (cancellable bool, phase string) { return true, "" }

// Run is the chain job.
func (c *Chainer) Run(in *ChainSubmission) jobs.Runner {
	steps := in.Steps
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		if err := c.requireState(ctx, in.Instance.ID, in.From); err != nil {
			return jobs.Failed(errcode.InvalidState, err)
		}
		for i, step := range steps {
			if jh.CancelRequested(ctx) {
				return jobs.Outcome{Status: jobs.StatusCancelled}
			}
			jh.Progress(ctx, i*100/len(steps), fmt.Sprintf("step %d of %d: %s", i+1, len(steps), step))
			err := c.runStep(ctx, jh, in, &step)
			switch {
			case err == nil:
				jh.Log(fmt.Sprintf("step %d done", i+1))
			case errors.Is(err, jobs.ErrCancelRequested):
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

// requireState fails when the instance is not in want. An empty want accepts any state.
func (c *Chainer) requireState(ctx context.Context, instanceID string, want instance.State) error {
	if want == "" {
		return nil
	}
	inst, err := c.fresh(ctx, instanceID)
	if err != nil {
		return err
	}
	if state := instance.State(inst.State); state != want {
		return fmt.Errorf("%w: it is %s and this needs it %s", errWrongState, state, want)
	}
	return nil
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
