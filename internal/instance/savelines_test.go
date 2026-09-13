package instance

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// stopped creates a fake container, runs it, and returns the engine and its id. The caller
// scripts the log before stopping it.
func stopped(t *testing.T) (*runtime.Fake, string, *runtime.FakeContainer) {
	t.Helper()
	rt := runtime.NewFake()
	id, err := rt.Create(t.Context(), &runtime.ContainerSpec{Image: "stub", User: containerUser})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := rt.Get(id)
	return rt, id, c
}

// TestAnEarlierAutosaveDoesNotConfirmThisStop is B2's real question. The server writes the
// completion literal on every save, autosaves included, so a line from earlier in the same
// session must not vouch for a stop that wrote nothing.
func TestAnEarlierAutosaveDoesNotConfirmThisStop(t *testing.T) {
	rt, id, c := stopped(t)

	c.Stdout("World save writing starting\n")
	c.Stdout("World save writing finished\n")

	// Everything after this instant is the stop's own evidence.
	time.Sleep(2 * time.Millisecond)
	cursor := time.Now()
	time.Sleep(2 * time.Millisecond)

	c.Stdout("Game - OnApplicationQuit\n")
	c.Stdout("World save writing starting\n")
	c.Exit(0)

	seen, err := SawSaveLine(t.Context(), rt, id, cursor)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if seen {
		t.Error("an autosave from earlier in the session confirmed a stop that never finished writing")
	}
}

// TestTheStopsOwnSaveLineConfirmsIt is the positive control: the same cursor must still accept
// a shutdown that did complete its save.
func TestTheStopsOwnSaveLineConfirmsIt(t *testing.T) {
	rt, id, c := stopped(t)

	c.Stdout("World save writing finished\n")
	time.Sleep(2 * time.Millisecond)
	cursor := time.Now()
	time.Sleep(2 * time.Millisecond)

	c.Stdout("Game - OnApplicationQuit\n")
	c.Stdout("World save writing finished\n")
	c.Exit(0)

	seen, err := SawSaveLine(t.Context(), rt, id, cursor)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if !seen {
		t.Error("a shutdown that completed its save was not confirmed")
	}
}

// TestASaveLineIsNeverAssembledAcrossStreams guards E5 on the lifecycle path. Docker frames are
// not lines, and stdout and stderr are separate streams: a fragment of each must never join into
// a literal neither of them wrote.
func TestASaveLineIsNeverAssembledAcrossStreams(t *testing.T) {
	rt, id, c := stopped(t)
	cursor := time.Now().Add(-time.Second)

	c.Stdout("World save ")
	c.Stderr("writing finished\n")
	c.Stdout("\n")
	c.Exit(0)

	seen, err := SawSaveLine(t.Context(), rt, id, cursor)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if seen {
		t.Error("a save line was assembled from two streams; neither carried one")
	}
}

// TestAnInterleavedStreamDoesNotHideASaveLine is the same defect's other direction: a fragment
// arriving on stderr mid-line must not split a genuine completion line on stdout.
func TestAnInterleavedStreamDoesNotHideASaveLine(t *testing.T) {
	rt, id, c := stopped(t)
	cursor := time.Now().Add(-time.Second)

	c.Stdout("World save writing ")
	c.Stderr("[some plugin chatter]\n")
	c.Stdout("finished\n")
	c.Exit(0)

	seen, err := SawSaveLine(t.Context(), rt, id, cursor)
	if err != nil {
		t.Fatalf("SawSaveLine: %v", err)
	}
	if !seen {
		t.Error("a completion line was hidden by an interleaved fragment on the other stream")
	}
}

// TestReadinessRejectsAContainerThatAlreadyExited is E6 from the other side. The ready line is
// evidence that the server got that far, not that it is still there: a process that announces
// itself and then dies must not make a start succeed.
func TestReadinessRejectsAContainerThatAlreadyExited(t *testing.T) {
	rt, id, c := stopped(t)

	c.Stdout("Game server connected\n")
	c.Exit(1)

	confirmed, err := AwaitReady(t.Context(), rt, id, time.Second, time.Second)
	if err == nil {
		t.Error("a container that printed the ready line and then exited was accepted as ready")
	}
	if confirmed {
		t.Error("confirmed = true for a container that is no longer running")
	}
}
