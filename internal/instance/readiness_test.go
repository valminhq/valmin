package instance

import (
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

func fakeContainer(t *testing.T, rt *runtime.Fake) string {
	t.Helper()
	id, err := rt.Create(t.Context(), &runtime.ContainerSpec{Name: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAwaitReadyConfirmsOnTheAnchoredLine is 12 §3.3's primary path: the line is enough on
// its own, and 03 §3.5 says nothing about a timestamp prefix being required to match it.
func TestAwaitReadyConfirmsOnTheAnchoredLine(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) {
		c.Stdout("08/20/2026 08:17:58: Game server connected\n")
	}
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	confirmed, err := AwaitReady(t.Context(), rt, id, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if !confirmed {
		t.Error("confirmed = false, want true: the anchored line was seen")
	}
}

// TestAwaitReadyFallsBackWithoutConfirming is ADR-043's fallback: still running, no exit,
// settle elapsed with no line — ready, but not confirmed.
func TestAwaitReadyFallsBackWithoutConfirming(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	confirmed, err := AwaitReady(t.Context(), rt, id, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if confirmed {
		t.Error("confirmed = true, but no line was ever written")
	}
}

// TestAwaitReadyErrorsWhenTheContainerExitsFirst is 12 §3.3: only an exit inside the window
// is a real start failure.
func TestAwaitReadyErrorsWhenTheContainerExitsFirst(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) { c.Exit(1) }
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	_, err := AwaitReady(t.Context(), rt, id, time.Second, time.Second)
	if err == nil {
		t.Fatal("AwaitReady: want an error, the container already exited")
	}
}

// TestAwaitReadyErrorsPastTheDeadline is the safety net: even with settle not yet elapsed,
// exceeding jobs.ready_timeout is a failure, not an indefinite wait.
func TestAwaitReadyErrorsPastTheDeadline(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	_, err := AwaitReady(t.Context(), rt, id, time.Hour, 5*time.Millisecond)
	if err == nil {
		t.Fatal("AwaitReady: want an error, the deadline is well before settle")
	}
}

// TestSawSaveLineMatchesTheFullLiteral is B2, checked at this call site: `finishing` shares
// the stem `finish` with `finished` and must not satisfy the pattern.
func TestSawSaveLineMatchesTheFullLiteral(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	rt.Get(id).Stdout("World save writing finishing\n")

	clean, err := SawSaveLine(t.Context(), rt, id)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Error("clean = true on `finishing`, want the full literal `finished` required")
	}

	rt.Get(id).Stdout("World save writing finished\n")
	clean, err = SawSaveLine(t.Context(), rt, id)
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Error("clean = false after the full literal was written")
	}
}

// TestLogTailDemuxesBothStreams proves the stdcopy demux path handles stdout and stderr
// without dropping either (03 §3.3's "no stdout/stderr deduplication").
func TestLogTailDemuxesBothStreams(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	rt.Get(id).Stdout("from stdout\n")
	rt.Get(id).Stderr("from stderr\n")

	tail, err := LogTail(t.Context(), rt, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"from stdout", "from stderr"} {
		if !strings.Contains(tail, want) {
			t.Errorf("tail %q missing %q", tail, want)
		}
	}
}
