package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// blockedJob submits a job that runs until the returned release is closed.
func blockedJob(t *testing.T, e *Engine, lock string) (job *store.Job, release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: lock},
		func(context.Context, *Handle) Outcome {
			<-release
			return Outcome{Status: StatusSucceeded}
		})
	if err != nil {
		t.Fatal(err)
	}
	return j, release
}

// TestWaitReturnsTheJobOnceItFinishes asserts a waiter wakes on the finish event and gets the
// terminal row.
func TestWaitReturnsTheJobOnceItFinishes(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	j, release := blockedJob(t, e, "instance:wait")

	got := make(chan *store.Job, 1)
	go func() {
		row, err := e.wait(t.Context(), j.ID, nil)
		if err != nil {
			t.Error(err)
		}
		got <- row
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case row := <-got:
		if row == nil || row.Status != StatusSucceeded {
			t.Fatalf("Wait = %+v, want a succeeded job", row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the job finished")
	}
}

// TestWaitReturnsAJobThatAlreadyFinished asserts a job that ended before Wait was called is
// returned without waiting for an event that already came.
func TestWaitReturnsAJobThatAlreadyFinished(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())
	j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:done"}, noop)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, db, j.ID)

	row, err := e.wait(t.Context(), j.ID, nil)
	if err != nil || row.Status != StatusSucceeded {
		t.Fatalf("Wait = %+v, %v, want a succeeded job", row, err)
	}
}

// TestWaitReadsTheRowWhenNoEventArrives asserts a job ended without a finish event, as recovery
// does, is still noticed by the periodic re-read.
func TestWaitReadsTheRowWhenNoEventArrives(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())
	e.waitRecheck = 20 * time.Millisecond
	j, release := blockedJob(t, e, "instance:silent")
	t.Cleanup(func() { close(release) })

	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := db.FinishJob(
			t.Context(), j.ID, StatusFailed, 0, nil, nil, nil, nil, time.Now(), nil,
		); err != nil {
			t.Error(err)
		}
	}()

	row, err := e.wait(t.Context(), j.ID, nil)
	if err != nil || row.Status != StatusFailed {
		t.Fatalf("Wait = %+v, %v, want the failed job", row, err)
	}
}

// TestWaitStopsWithItsContext asserts a waiter gives up when its context ends.
func TestWaitStopsWithItsContext(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	j, release := blockedJob(t, e, "instance:ctx")
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.wait(ctx, j.ID, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want the context's deadline", err)
	}
}

// TestWaitForAMissingJobFails asserts an unknown id is an error and not a wait.
func TestWaitForAMissingJobFails(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	if _, err := e.wait(t.Context(), "nope", nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Wait = %v, want ErrJobNotFound", err)
	}
}

// TestHandleWaitsEndWhenTheJobIsCancelled asserts both of a running job's waits return
// ErrCancelRequested as soon as Engine.Cancel flags it.
func TestHandleWaitsEndWhenTheJobIsCancelled(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())
	e.RegisterCancelPolicy(KindChain, func(string) (bool, string) { return true, "" })
	child, releaseChild := blockedJob(t, e, "instance:child")
	t.Cleanup(func() { close(releaseChild) })

	results := make(chan error, 2)
	started := make(chan struct{})
	parent, err := e.Submit(t.Context(), &Spec{Kind: KindChain, LockKey: "chain:parent"},
		func(ctx context.Context, h *Handle) Outcome {
			close(started)
			_, err := h.WaitJob(ctx, child.ID)
			results <- err
			results <- h.Sleep(ctx, time.Hour)
			return Outcome{Status: StatusCancelled}
		})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	if err := e.Cancel(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}

	for i, what := range []string{"WaitJob", "Sleep"} {
		select {
		case err := <-results:
			if !errors.Is(err, ErrCancelRequested) {
				t.Errorf("%s = %v, want ErrCancelRequested", what, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s (%d) did not return after the cancel", what, i)
		}
	}
}

// TestHandleSleepEndsAfterItsDuration asserts a sleep with no cancel just waits.
func TestHandleSleepEndsAfterItsDuration(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	got := make(chan error, 1)
	j, err := e.Submit(t.Context(), &Spec{Kind: KindChain, LockKey: "chain:sleep"},
		func(ctx context.Context, h *Handle) Outcome {
			got <- h.Sleep(ctx, 10*time.Millisecond)
			return Outcome{Status: StatusSucceeded}
		})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Sleep = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Sleep did not return in job %s", j.ID)
	}
}
