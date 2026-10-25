package control

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// TestValidateSteps asserts which fields a refused step list names.
func TestValidateSteps(t *testing.T) {
	tooMany := make([]ChainStep, MaxChainSteps+1)
	for i := range tooMany {
		tooMany[i] = ChainStep{Type: StepStop}
	}
	tests := []struct {
		name  string
		steps []ChainStep
		want  []string
	}{
		{"no steps", nil, []string{"steps"}},
		{"too many steps", tooMany, []string{"steps"}},
		{
			"every lifecycle step",
			[]ChainStep{
				{Type: StepBackup},
				{Type: StepStop},
				{Type: StepGameUpdate},
				{Type: StepStart},
				{Type: StepRestart},
			},
			nil,
		},
		{"wait in range", []ChainStep{{Type: StepWait, Seconds: 300}}, nil},
		{"wait of nothing", []ChainStep{{Type: StepWait}}, []string{"steps[0].seconds"}},
		{"wait past an hour", []ChainStep{{Type: StepWait, Seconds: MaxWaitSeconds + 1}}, []string{"steps[0].seconds"}},
		{"wait for ready", []ChainStep{{Type: StepWaitFor, Condition: ConditionReady, Seconds: 120}}, nil},
		{"wait for empty", []ChainStep{{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 600}}, nil},
		{"wait for nothing", []ChainStep{{Type: StepWaitFor, Seconds: 30}}, []string{"steps[0].condition"}},
		{
			"wait for an unknown condition",
			[]ChainStep{{Type: StepWaitFor, Condition: "sunrise", Seconds: 30}},
			[]string{"steps[0].condition"},
		},
		{
			"wait for with no limit",
			[]ChainStep{{Type: StepWaitFor, Condition: ConditionReady}},
			[]string{"steps[0].seconds"},
		},
		{
			"wait for past an hour",
			[]ChainStep{{Type: StepWaitFor, Condition: ConditionReady, Seconds: MaxWaitSeconds + 1}},
			[]string{"steps[0].seconds"},
		},
		{"command", []ChainStep{{Type: StepCommand, Command: "save"}}, nil},
		{"empty command", []ChainStep{{Type: StepCommand, Command: "  "}}, []string{"steps[0].command"}},
		{"two lines", []ChainStep{{Type: StepCommand, Command: "save\nban x"}}, []string{"steps[0].command"}},
		{
			"oversized command",
			[]ChainStep{{Type: StepCommand, Command: strings.Repeat("a", 1025)}},
			[]string{"steps[0].command"},
		},
		{"unknown type", []ChainStep{{Type: "reboot"}}, []string{"steps[0].type"}},
		{
			"every bad step is named",
			[]ChainStep{{Type: StepWait}, {Type: StepStop}, {Type: "reboot"}},
			[]string{"steps[0].seconds", "steps[2].type"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, p := range ValidateSteps(tt.steps) {
				got = append(got, p.Field)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ValidateSteps problems = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateStepsTrimsCommands asserts a command is stored without its surrounding space.
func TestValidateStepsTrimsCommands(t *testing.T) {
	steps := []ChainStep{{Type: StepCommand, Command: "  save "}}
	if problems := ValidateSteps(steps); len(problems) != 0 {
		t.Fatalf("ValidateSteps = %v, want none", problems)
	}
	if steps[0].Command != "save" {
		t.Errorf("command = %q, want %q", steps[0].Command, "save")
	}
}

// TestStopsServer asserts which step lists take a running server down.
func TestStopsServer(t *testing.T) {
	tests := []struct {
		name  string
		steps []ChainStep
		want  bool
	}{
		{"commands and waits", []ChainStep{{Type: StepCommand, Command: "save"}, {Type: StepWait, Seconds: 5}}, false},
		{"start only", []ChainStep{{Type: StepStart}}, false},
		{"wait for ready", []ChainStep{{Type: StepWaitFor, Condition: ConditionReady, Seconds: 60}}, false},
		{"stop", []ChainStep{{Type: StepStop}}, true},
		{"backup", []ChainStep{{Type: StepBackup}}, true},
		{"restart after a warning", []ChainStep{{Type: StepCommand, Command: "save"}, {Type: StepRestart}}, true},
		{"game update", []ChainStep{{Type: StepGameUpdate}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StopsServer(tt.steps); got != tt.want {
				t.Errorf("StopsServer = %v, want %v", got, tt.want)
			}
		})
	}
}

// chainWorld seeds inst-a in state with a server that is ready as soon as it starts, and a
// chainer that polls quickly.
func chainWorld(t *testing.T, state instance.State) (*world, *fakeSender) {
	t.Helper()
	w := newWorld(t)
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	seedInstance(t, w, string(state))
	sender := &fakeSender{}
	w.c.Chainer.Commands = sender
	w.c.Chainer.Engine.RegisterCancelPolicy(jobs.KindChain, ChainCancelPolicy)
	return w, sender
}

func submitChain(t *testing.T, w *world, steps ...ChainStep) (*store.Job, error) {
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		return nil, err
	}
	return w.c.Chainer.Submit(t.Context(), &ChainSubmission{
		Instance: inst, Steps: steps, Kind: jobs.KindChain, Payload: ChainPayload{Steps: steps},
	})
}

// awaitJob waits for a job to end and returns its row.
func awaitJob(t *testing.T, w *world, id string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := w.db.JobByID(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "queued" && got.Status != "running" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return nil
}

// containerOf is the id of the seeded instance's container.
func containerOf(t *testing.T, w *world) string {
	t.Helper()
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	return deref(inst.ContainerID)
}

func instanceState(t *testing.T, w *world) instance.State {
	t.Helper()
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	return instance.State(inst.State)
}

// TestChainRunsItsStepsInOrder asserts a command, a stop and a start run one after another and
// leave the server running.
func TestChainRunsItsStepsInOrder(t *testing.T) {
	w, sender := chainWorld(t, instance.StateRunning)
	runs := w.fake.Runs()

	job, err := submitChain(t, w,
		ChainStep{Type: StepCommand, Command: "save"}, ChainStep{Type: StepStop}, ChainStep{Type: StepStart})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if !slices.Equal(sender.sent, []string{"save"}) {
		t.Errorf("sent %q, want save once", sender.sent)
	}
	if state := instanceState(t, w); state != instance.StateRunning {
		t.Errorf("instance state = %s, want running", state)
	}
	if w.fake.Runs() != runs+1 {
		t.Errorf("server started %d times by the chain, want once", w.fake.Runs()-runs)
	}
}

// TestChainStopsAtTheFirstFailedStep asserts a step that cannot run fails the chain with the
// reason, and that nothing after it runs.
func TestChainStopsAtTheFirstFailedStep(t *testing.T) {
	w, sender := chainWorld(t, instance.StateRunning)

	job, err := submitChain(t, w, ChainStep{Type: StepGameUpdate}, ChainStep{Type: StepCommand, Command: "save"})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusFailed || deref(got.ErrorCode) != errcode.InvalidState.String() {
		t.Fatalf("chain = %s %q, want failed invalid_state", got.Status, deref(got.ErrorCode))
	}
	if !strings.Contains(deref(got.Error), "step 1 of 2 (game_update)") {
		t.Errorf("error %q does not name the failed step", deref(got.Error))
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent %q after a failed step", sender.sent)
	}
	if state := instanceState(t, w); state != instance.StateRunning {
		t.Errorf("instance state = %s, want running", state)
	}
}

// TestChainFailsWithTheCodeOfAFailedJob asserts a step's job that fails ends the chain with that
// job's code, before the next step.
func TestChainFailsWithTheCodeOfAFailedJob(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)
	w.fake.StopErr = errors.New("daemon unreachable")
	runs := w.fake.Runs()

	job, err := submitChain(t, w, ChainStep{Type: StepStop}, ChainStep{Type: StepStart})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusFailed || deref(got.ErrorCode) != errcode.Internal.String() {
		t.Fatalf("chain = %s %q, want failed internal", got.Status, deref(got.ErrorCode))
	}
	if !strings.Contains(deref(got.Error), "step 1 of 2 (stop)") {
		t.Errorf("error %q does not name the failed step", deref(got.Error))
	}
	if w.fake.Runs() != runs {
		t.Error("the start step ran after the stop failed")
	}
}

// TestChainSkipsWhatAStoppedServerCannotDo asserts a command is skipped and a stop is already
// satisfied when the server is stopped.
func TestChainSkipsWhatAStoppedServerCannotDo(t *testing.T) {
	w, sender := chainWorld(t, instance.StateStopped)

	job, err := submitChain(t, w, ChainStep{Type: StepCommand, Command: "save"}, ChainStep{Type: StepStop})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if len(sender.sent) != 0 {
		t.Errorf("sent %q to a stopped server", sender.sent)
	}
	if state := instanceState(t, w); state != instance.StateStopped {
		t.Errorf("instance state = %s, want stopped", state)
	}
}

// TestChainSkipsACommandAServerCannotTake asserts a command refused because the container is not
// running is skipped, so a crashed server does not stop the steps after it.
func TestChainSkipsACommandAServerCannotTake(t *testing.T) {
	w, sender := chainWorld(t, instance.StateRunning)
	sender.err = command.ErrInvalidState

	job, err := submitChain(t, w, ChainStep{Type: StepCommand, Command: "save"}, ChainStep{Type: StepStop})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if state := instanceState(t, w); state != instance.StateStopped {
		t.Errorf("instance state = %s, want stopped: the step after the skipped command must run", state)
	}
}

// TestChainCancelsDuringAWaitAndHoldsItsLock asserts a second chain on the instance is refused
// while one runs, and that cancelling ends the wait without running the steps after it.
func TestChainCancelsDuringAWaitAndHoldsItsLock(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)

	job, err := submitChain(t, w, ChainStep{Type: StepWait, Seconds: 60}, ChainStep{Type: StepStop})
	if err != nil {
		t.Fatal(err)
	}
	_, err = submitChain(t, w, ChainStep{Type: StepWait, Seconds: 60})
	var conflict *store.JobConflict
	if !errors.As(err, &conflict) || conflict.JobID != job.ID {
		t.Fatalf("second chain error = %v, want a conflict with %s", err, job.ID)
	}
	if err := w.c.Chainer.Engine.Cancel(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusCancelled {
		t.Errorf("chain status = %s, want cancelled", got.Status)
	}
	if state := instanceState(t, w); state != instance.StateRunning {
		t.Errorf("instance state = %s, want running: the stop after the wait must not run", state)
	}
}

// TestChainCommandRetriesWhileTheListenerComesUp asserts a command that fails because the
// server's RCON listener is not up yet is sent again, and is sent once it is.
func TestChainCommandRetriesWhileTheListenerComesUp(t *testing.T) {
	w, sender := chainWorld(t, instance.StateRunning)
	var loaded atomic.Bool
	sender.loaded = &loaded
	go func() {
		time.Sleep(100 * time.Millisecond)
		loaded.Store(true)
	}()

	job, err := submitChain(t, w, ChainStep{Type: StepCommand, Command: "save"})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if !slices.Equal(sender.sent, []string{"save"}) {
		t.Errorf("sent %q, want save once", sender.sent)
	}
}

// TestWaitForReadyReturnsOnceTheServerIsReady asserts a wait for ready ends as soon as the
// boot has logged its ready line.
func TestWaitForReadyReturnsOnceTheServerIsReady(t *testing.T) {
	w, _ := chainWorld(t, instance.StateStopped)

	job, err := submitChain(t, w,
		ChainStep{Type: StepStart}, ChainStep{Type: StepWaitFor, Condition: ConditionReady, Seconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	begun := time.Now()
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if took := time.Since(begun); took > 30*time.Second {
		t.Errorf("chain took %v, want it to end when the server was ready", took)
	}
	if !strings.Contains(deref(got.Log), "ready") {
		t.Errorf("log %q does not say the server was ready", deref(got.Log))
	}
}

// TestWaitForReadyContinuesAfterItsLimit asserts a server that never says it is ready delays
// the chain by the limit and no longer.
func TestWaitForReadyContinuesAfterItsLimit(t *testing.T) {
	w, sender := chainWorld(t, instance.StateStopped)
	w.fake.OnStart = nil

	job, err := submitChain(t, w,
		ChainStep{Type: StepStart},
		ChainStep{Type: StepWaitFor, Condition: ConditionReady, Seconds: 1},
		ChainStep{Type: StepCommand, Command: "save"})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if !strings.Contains(deref(got.Log), "did not report") {
		t.Errorf("log %q does not warn that the server never reported ready", deref(got.Log))
	}
	if !slices.Equal(sender.sent, []string{"save"}) {
		t.Errorf("sent %q, want the step after the wait to run", sender.sent)
	}
}

// TestWaitForIsSkippedOnAStoppedServer asserts a wait for a condition a stopped server cannot
// reach does not hold the chain.
func TestWaitForIsSkippedOnAStoppedServer(t *testing.T) {
	w, _ := chainWorld(t, instance.StateStopped)

	job, err := submitChain(t, w, ChainStep{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if !strings.Contains(deref(got.Log), "skipped") {
		t.Errorf("log %q does not say the wait was skipped", deref(got.Log))
	}
}

// TestWaitForEmptyEndsWhenTheLastPlayerLeaves asserts the wait follows the server's own player
// count and ends the moment it reads zero.
func TestWaitForEmptyEndsWhenTheLastPlayerLeaves(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)
	container := w.fake.Get(containerOf(t, w))
	container.Stdout("Connections 2 ZDOS:1  sent:0 recv:0\n")

	job, err := submitChain(t, w, ChainStep{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if row, err := w.db.JobByID(t.Context(), job.ID); err != nil || row.Status != "running" {
		t.Fatalf("job = %+v, %v, want it still waiting while two players are connected", row, err)
	}
	container.Stdout("Connections 0 ZDOS:1  sent:0 recv:0\n")

	got := awaitJob(t, w, job.ID)
	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
}

// TestWaitForEmptyContinuesAfterItsLimit asserts players who never leave delay the chain by
// the limit and no longer.
func TestWaitForEmptyContinuesAfterItsLimit(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)
	w.fake.Get(containerOf(t, w)).Stdout("Connections 1 ZDOS:1  sent:0 recv:0\n")

	job, err := submitChain(t, w, ChainStep{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusSucceeded {
		t.Fatalf("chain status = %s (%v), want succeeded", got.Status, deref(got.Error))
	}
	if !strings.Contains(deref(got.Log), "players") {
		t.Errorf("log %q does not say players were still connected", deref(got.Log))
	}
}

// TestWaitForCancelsWithTheChain asserts cancelling a chain ends a wait for a condition.
func TestWaitForCancelsWithTheChain(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)
	w.fake.Get(containerOf(t, w)).Stdout("Connections 1 ZDOS:1  sent:0 recv:0\n")

	job, err := submitChain(t, w,
		ChainStep{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 600}, ChainStep{Type: StepStop})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := w.c.Chainer.Engine.Cancel(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusCancelled {
		t.Errorf("chain status = %s, want cancelled", got.Status)
	}
	if state := instanceState(t, w); state != instance.StateRunning {
		t.Errorf("instance state = %s, want running: the stop after the wait must not run", state)
	}
}

// TestWaitForEmptyFailsWhenTheServerStops asserts a wait for no players does not sit out its
// limit when the server stops under it.
func TestWaitForEmptyFailsWhenTheServerStops(t *testing.T) {
	w, _ := chainWorld(t, instance.StateRunning)
	w.fake.Get(containerOf(t, w)).Stdout("Connections 1 ZDOS:1  sent:0 recv:0\n")

	job, err := submitChain(t, w, ChainStep{Type: StepWaitFor, Condition: ConditionEmpty, Seconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	seed(t, w.db, `UPDATE instances SET state = 'stopped' WHERE id = ?`, seededInstanceID)
	w.c.Chainer.states.notify(seededInstanceID)

	got := awaitJob(t, w, job.ID)
	if got.Status != jobs.StatusFailed || deref(got.ErrorCode) != errcode.InvalidState.String() {
		t.Fatalf("chain = %s %q, want failed invalid_state", got.Status, deref(got.ErrorCode))
	}
}
