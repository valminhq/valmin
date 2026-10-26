package instance

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

func fakeContainer(t *testing.T, rt *runtime.Fake) string {
	t.Helper()
	id, err := rt.Create(t.Context(), &runtime.ContainerSpec{User: containerUser, Name: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSawSaveLineMatchesTheFullLiteral is B2, checked at this call site: `finishing` shares
// the stem `finish` with `finished` and must not satisfy the pattern.
func TestSawSaveLineMatchesTheFullLiteral(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	rt.Get(id).Stdout("World save writing finishing\n")

	clean, err := NewContainerLogs(rt, DefaultPatterns).SawSaveLine(t.Context(), id, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Error("clean = true on `finishing`, want the full literal `finished` required")
	}

	rt.Get(id).Stdout("World save writing finished\n")
	clean, err = NewContainerLogs(rt, DefaultPatterns).SawSaveLine(t.Context(), id, time.Time{})
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

// TestSawSaveLineIgnoresAnEarlierBoot is the same scoping rule on the stop path, and it
// matters more: an unscoped search finds the *previous* stop's save-complete literal and
// reports a clean shutdown for a stop that never wrote one (B2, from the other direction).
// The stop's own cursor is what excludes it, and excludes an autosave earlier in this boot
// too — see TestAnEarlierAutosaveDoesNotConfirmThisStop.
func TestSawSaveLineIgnoresAnEarlierBoot(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rt.Get(id).Stdout("World save writing finished\n")

	time.Sleep(2 * time.Millisecond)
	stopRequested := time.Now()
	time.Sleep(2 * time.Millisecond)

	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rt.Get(id).Stdout("World save writing finishing\n")

	saw, err := NewContainerLogs(rt, DefaultPatterns).SawSaveLine(t.Context(), id, stopRequested)
	if err != nil {
		t.Fatal(err)
	}
	if saw {
		t.Error("SawSaveLine = true, but this boot only reached \"finishing\"")
	}
}

// TestReadyLoggedSeesOnlyThisBoot asserts the ready line counts only when the current boot
// printed it.
func TestReadyLoggedSeesOnlyThisBoot(t *testing.T) {
	tests := []struct {
		name      string
		earlier   string
		this      string
		wantReady bool
	}{
		{"this boot", "", "Game server connected\n", true},
		{"earlier boot only", "Game server connected\n", "Zonesystem Start 1\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := runtime.NewFake()
				id := fakeContainer(t, rt)
				if err := rt.Start(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				rt.Get(id).Stdout(tt.earlier)
				time.Sleep(2 * bootStartMargin)
				if err := rt.Start(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				rt.Get(id).Stdout(tt.this)

				ready, err := NewContainerLogs(rt, DefaultPatterns).ReadyLogged(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if ready != tt.wantReady {
					t.Errorf("ReadyLogged = %v, want %v", ready, tt.wantReady)
				}
			})
		})
	}
}
