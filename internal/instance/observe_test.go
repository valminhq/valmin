package instance

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
)

var (
	gone    = Reality{}
	up      = Reality{Found: true, Running: true}
	exited  = Reality{Found: true}
	oom     = Reality{Found: true, OOMKilled: true}
	looping = Reality{Found: true, Running: true, CrashLooping: true}
)

// TestObserveCoversTheRecoveryMatrix is WP-M1-15's central obligation: every row of
// 12 §9.2, plus 12 §2.2's four observation rows, asserted against the table rather than
// spot-checked. A row that is not here is a row that has never run.
func TestObserveCoversTheRecoveryMatrix(t *testing.T) {
	tests := []struct {
		name  string
		state State
		real  Reality
		want  Verdict
	}{
		// 12 §2.2's observations (08 §6.1 step 3's first four bullets).
		{"running, container gone", StateRunning, gone, Verdict{To: StateError}},
		{"running, container exited cleanly", StateRunning, exited, Verdict{To: StateStopped}},
		{"running, OOM-killed", StateRunning, oom, Verdict{To: StateError, Stop: true}},
		{"running, crash looping", StateRunning, looping, Verdict{To: StateError, Stop: true}},
		{"running, still running", StateRunning, up, Verdict{}},
		{"stopped, container running", StateStopped, up, Verdict{To: StateRunning}},
		{"stopped, container stopped", StateStopped, exited, Verdict{}},
		{"stopped, container gone", StateStopped, gone, Verdict{}},

		// 12 §9.2, row for row.
		{"provisioning, none", StateProvisioning, gone, Verdict{Rerun: jobs.KindProvision, To: StateError}},
		{"provisioning, partial", StateProvisioning, exited, Verdict{Rerun: jobs.KindProvision, To: StateError}},
		{"starting, running", StateStarting, up, Verdict{To: StateRunning, Recheck: true}},
		{"starting, not running", StateStarting, exited, Verdict{To: StateStopped}},
		{"starting, gone", StateStarting, gone, Verdict{To: StateStopped}},
		{"stopping, exited", StateStopping, exited, Verdict{To: StateStopped}},
		{"stopping, running", StateStopping, up, Verdict{To: StateError}},
		{"backing_up, not running", StateBackingUp, exited, Verdict{To: StateStopped}},
		{"backing_up, running (hot copy)", StateBackingUp, up, Verdict{To: StateRunning}},
		{"restoring, running", StateRestoring, up, Verdict{To: StateError}},
		{"restoring, exited", StateRestoring, exited, Verdict{To: StateError}},
		{"restoring, gone", StateRestoring, gone, Verdict{To: StateError}},
		{"updating, running", StateUpdating, up, Verdict{To: StateStopped}},
		{"updating, gone", StateUpdating, gone, Verdict{To: StateStopped}},
		{"deleting, running", StateDeleting, up, Verdict{Rerun: jobs.KindDelete}},
		{"deleting, gone", StateDeleting, gone, Verdict{Rerun: jobs.KindDelete}},

		// The two states the observer must never move.
		{"created is never moved", StateCreated, gone, Verdict{}},
		{"error is a parking state", StateError, up, Verdict{}},
		{"error stays parked even when the container is gone", StateError, gone, Verdict{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Observe(tc.state, tc.real)
			// Reason is prose; every other field is the decision.
			got.Reason = ""
			if got != tc.want {
				t.Errorf("Observe(%s, %+v) = %+v, want %+v", tc.state, tc.real, got, tc.want)
			}
		})
	}
}

// TestObserveNeverProducesAnUndocumentedTransition ties the matrix to 12 §2.2's own table:
// a verdict the transition table rejects would be a write the state machine forbids, which
// is exactly the class of bug two separate tables exist to catch.
func TestObserveNeverProducesAnUndocumentedTransition(t *testing.T) {
	realities := []Reality{gone, up, exited, oom, looping}
	for _, from := range allStates {
		for _, r := range realities {
			v := Observe(from, r)
			if v.To == "" || v.To == from {
				continue
			}
			if !Valid(from, v.To) {
				t.Errorf("Observe(%s, %+v) wants %s, which 12 §2.2 does not permit", from, r, v.To)
			}
		}
	}
}

// TestObserveNeverAutoStartsAfterARestore is B7, given its own test because it is the row
// most likely to be "improved" into a helpful auto-recovery: a restore whose outcome is
// unknown must never be followed by a start, whatever Docker says.
func TestObserveNeverAutoStartsAfterARestore(t *testing.T) {
	for _, r := range []Reality{gone, up, exited, oom, looping} {
		if got := Observe(StateRestoring, r); got.To != StateError {
			t.Errorf("Observe(restoring, %+v).To = %s, want error", r, got.To)
		}
	}
	for _, r := range []Reality{gone, up, exited} {
		if got := Observe(StateUpdating, r); got.To == StateRunning {
			t.Errorf("Observe(updating, %+v) wants running — 12 §9.2 says never auto-start", r)
		}
	}
}

// TestObserveStopsTheContainerItParksInError is 08 §6's guard against `unless-stopped`.
// Parking the row is only half of it: Docker will resurrect the container otherwise.
func TestObserveStopsTheContainerItParksInError(t *testing.T) {
	for _, r := range []Reality{oom, looping} {
		got := Observe(StateRunning, r)
		if got.To != StateError || !got.Stop {
			t.Errorf("Observe(running, %+v) = %+v, want error with Stop", r, got)
		}
	}
	// A container that merely exited is not resurrection bait and must not be signalled.
	if got := Observe(StateRunning, exited); got.Stop {
		t.Error("a cleanly exited container should not be stopped again")
	}
}

func TestCrashLoopNeedsThresholdWithinWindow(t *testing.T) {
	now := time.Now()
	c := NewCrashLoop()

	if c.Looping("a", 0, now) {
		t.Fatal("the first observation cannot be a loop: it is only a baseline")
	}
	if c.Looping("a", CrashLoopThreshold-1, now.Add(time.Minute)) {
		t.Error("below the threshold reported a loop")
	}
	if !c.Looping("a", CrashLoopThreshold, now.Add(2*time.Minute)) {
		t.Error("threshold restarts inside the window did not report a loop")
	}
}

// TestCrashLoopRebasesOutsideTheWindow is why the window exists at all: Docker's
// RestartCount is cumulative, so a server restarted three times last spring must not be
// parked in `error` on this boot.
func TestCrashLoopRebasesOutsideTheWindow(t *testing.T) {
	now := time.Now()
	c := NewCrashLoop()

	c.Looping("a", 0, now)
	if c.Looping("a", 100, now.Add(CrashLoopWindow+time.Second)) {
		t.Error("a count that grew outside the window reported a loop")
	}
	// Having rebased at 100, the next threshold is 100 + threshold.
	if c.Looping("a", 100+CrashLoopThreshold-1, now.Add(CrashLoopWindow+2*time.Second)) {
		t.Error("rebasing did not reset the threshold")
	}
	if !c.Looping("a", 100+CrashLoopThreshold, now.Add(CrashLoopWindow+3*time.Second)) {
		t.Error("threshold restarts after a rebase did not report a loop")
	}
}

// TestCrashLoopRebasesWhenTheCountFalls covers a panel-initiated start, which is the one
// thing that resets Docker's own counter.
func TestCrashLoopRebasesWhenTheCountFalls(t *testing.T) {
	now := time.Now()
	c := NewCrashLoop()

	c.Looping("a", 9, now)
	if c.Looping("a", 0, now.Add(time.Minute)) {
		t.Error("a counter reset reported a loop")
	}
	if !c.Looping("a", CrashLoopThreshold, now.Add(2*time.Minute)) {
		t.Error("the guard stopped working after a counter reset")
	}
}
