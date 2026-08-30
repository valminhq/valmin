package instance

import (
	"testing"

	"github.com/valminhq/valmin/internal/runtime"
)

func TestReconcileNoContainerIDIsStopped(t *testing.T) {
	got, err := Reconcile(t.Context(), runtime.NewFake(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != StateStopped {
		t.Errorf("got %s, want stopped", got)
	}
}

func TestReconcileMissingContainerIsStopped(t *testing.T) {
	got, err := Reconcile(t.Context(), runtime.NewFake(), "no-such-container")
	if err != nil {
		t.Fatal(err)
	}
	if got != StateStopped {
		t.Errorf("got %s, want stopped", got)
	}
}

func TestReconcileRunningContainerIsRunning(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	got, err := Reconcile(t.Context(), fake, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != StateRunning {
		t.Errorf("got %s, want running", got)
	}
}

// TestReconcileExitedContainerIsStopped covers Docker being consulted rather than assumed:
// the container exists but is not running.
func TestReconcileExitedContainerIsStopped(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	fake.Get(id).Exit(0)

	got, err := Reconcile(t.Context(), fake, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != StateStopped {
		t.Errorf("got %s, want stopped", got)
	}
}
