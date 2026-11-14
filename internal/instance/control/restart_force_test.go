package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
)

// TestRestartAfterAKillStartsOnlyWhenForced asserts a restart whose stop ended in Docker's
// SIGKILL parks the server in error, as always, unless the restart was forced: then the server
// starts again and the job records that the world was not saved.
func TestRestartAfterAKillStartsOnlyWhenForced(t *testing.T) {
	tests := []struct {
		name      string
		force     bool
		wantJob   string
		wantState instance.State
	}{
		{"not forced", false, jobs.StatusFailed, instance.StateError},
		{"forced", true, jobs.StatusSucceeded, instance.StateRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
			w.fake.OnStop = func(c *runtime.FakeContainer) { c.Exit(137) }
			containerID := seedInstance(t, w, string(instance.StateRunning))

			inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := w.c.Restarter.Submit(t.Context(), &RestartSubmission{
				Instance: inst, ContainerID: containerID, ForceStart: tt.force,
			})
			if err != nil {
				t.Fatalf("submit restart: %v", err)
			}
			got := awaitJob(t, w, job.ID)

			if got.Status != tt.wantJob {
				t.Fatalf("restart status = %s (%v), want %s", got.Status, deref(got.Error), tt.wantJob)
			}
			if state := instanceState(t, w); state != tt.wantState {
				t.Errorf("state = %s, want %s", state, tt.wantState)
			}
			if tt.force {
				if got.Clean == nil || *got.Clean {
					t.Errorf("clean = %v, want false: a killed server did not save", got.Clean)
				}
				if !strings.Contains(deref(got.Log), "force-killed") {
					t.Errorf("job log = %q, want it to say the stop was force-killed", deref(got.Log))
				}
			}
		})
	}
}

// staysRunning is a runtime whose Stop takes the whole timeout and returns with the container
// still running, as a daemon that reports a stop it did not complete would.
type staysRunning struct{ *runtime.Fake }

func (staysRunning) Stop(_ context.Context, _, _ string, timeout time.Duration) error {
	time.Sleep(timeout)
	return nil
}

// TestAForcedRestartDoesNotStartAContainerThatIsStillRunning asserts that a forced restart whose
// stop ended with the container still running parks the server in error instead of reporting a
// restart of a server that never went down.
func TestAForcedRestartDoesNotStartAContainerThatIsStillRunning(t *testing.T) {
	w := newWorld(t)
	w.fake.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	containerID := seedInstance(t, w, string(instance.StateRunning))
	w.c.Stopper.Runtime = staysRunning{w.fake}
	w.c.Stopper.StopTimeout = 20 * time.Millisecond

	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := w.c.Restarter.Submit(t.Context(), &RestartSubmission{
		Instance: inst, ContainerID: containerID, ForceStart: true,
	})
	if err != nil {
		t.Fatalf("submit restart: %v", err)
	}
	got := awaitJob(t, w, job.ID)

	if got.Status != jobs.StatusFailed {
		t.Fatalf("restart status = %s (%v), want failed", got.Status, deref(got.Error))
	}
	if state := instanceState(t, w); state != instance.StateError {
		t.Errorf("state = %s, want error", state)
	}
}
