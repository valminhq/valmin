package instance

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// labelledContainer creates a container carrying the instance label a panel container has.
func labelledContainer(t *testing.T, rt *runtime.Fake, cmd ...string) string {
	t.Helper()
	id, err := rt.Create(t.Context(), &runtime.ContainerSpec{
		User: containerUser, Name: "fake", Cmd: cmd,
		Labels: map[string]string{LabelInstanceID: "inst-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// startContainer starts the container id.
func startContainer(t *testing.T, rt *runtime.Fake, id string) {
	t.Helper()
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
}

// TestAwaitReadyConfirmsOnTheAnchoredLine asserts the ready line is enough on its own, with or
// without the game's timestamp prefix.
func TestAwaitReadyConfirmsOnTheAnchoredLine(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) {
		c.Stdout("08/20/2026 08:17:58: Game server connected\n")
	}
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	confirmed, err := streams.AwaitReady(t.Context(), id, 5*time.Second)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if !confirmed {
		t.Error("confirmed = false, want true: the anchored line was seen")
	}
}

// TestAwaitReadyWakesWhenTheLineArrives asserts a wait that started before the line returns when
// it is logged, not when the settle time runs out.
func TestAwaitReadyWakesWhenTheLineArrives(t *testing.T) {
	rt := runtime.NewFake()
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	go func() {
		time.Sleep(50 * time.Millisecond)
		rt.Get(id).Stdout("Game server connected\n")
	}()
	begun := time.Now()
	confirmed, err := streams.AwaitReady(t.Context(), id, time.Minute)
	if err != nil || !confirmed {
		t.Fatalf("AwaitReady = %v, %v, want confirmed", confirmed, err)
	}
	if took := time.Since(begun); took > 10*time.Second {
		t.Errorf("AwaitReady took %v, want it to return when the line arrived", took)
	}
}

// TestAwaitReadyWaitsForTheLobbyOnCrossplay asserts a crossplay server is confirmed by its
// PlayFab lobby line and never by the earlier Steam connection line alone.
func TestAwaitReadyWaitsForTheLobbyOnCrossplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		log           string
		wantConfirmed bool
	}{
		{"steam connection only", "Game server connected\n", false},
		{"lobby created", "Game server connected\nCreated PlayFab lobby with ID \"x\"\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := runtime.NewFake()
			rt.OnStart = func(c *runtime.FakeContainer) { c.Stdout(tt.log) }
			id := labelledContainer(t, rt, "-crossplay")
			startContainer(t, rt, id)
			streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
			defer streams.Shutdown()

			confirmed, err := streams.AwaitReady(t.Context(), id, 100*time.Millisecond)
			if err != nil {
				t.Fatalf("AwaitReady: %v", err)
			}
			if confirmed != tt.wantConfirmed {
				t.Errorf("confirmed = %v, want %v", confirmed, tt.wantConfirmed)
			}
		})
	}
}

// TestAwaitReadyFallsBackWithoutConfirming asserts a container that is still running when the
// settle time ends is ready but not confirmed.
func TestAwaitReadyFallsBackWithoutConfirming(t *testing.T) {
	t.Parallel()
	rt := runtime.NewFake()
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	confirmed, err := streams.AwaitReady(t.Context(), id, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitReady: %v", err)
	}
	if confirmed {
		t.Error("confirmed = true, but no line was ever written")
	}
}

// TestAwaitReadyErrorsWhenTheContainerExitsFirst asserts only an exit inside the window is a
// start failure.
func TestAwaitReadyErrorsWhenTheContainerExitsFirst(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) { c.Exit(1) }
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	_, err := streams.AwaitReady(t.Context(), id, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("AwaitReady = %v, want an error saying the container exited", err)
	}
}

// TestAwaitReadyRejectsALineFromAContainerThatThenDied asserts a server that announced itself
// and exited is not ready.
func TestAwaitReadyRejectsALineFromAContainerThatThenDied(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) {
		c.Stdout("Game server connected\n")
		c.Exit(1)
	}
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	confirmed, err := streams.AwaitReady(t.Context(), id, 5*time.Second)
	if err == nil || confirmed {
		t.Fatalf("AwaitReady = %v, %v, want an error for a container that died after the line", confirmed, err)
	}
}

// TestAwaitReadyNeedsAnInstanceLabel asserts a container the panel did not label is refused
// rather than read under a made-up instance.
func TestAwaitReadyNeedsAnInstanceLabel(t *testing.T) {
	rt := runtime.NewFake()
	id := fakeContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	if _, err := streams.AwaitReady(t.Context(), id, time.Second); err == nil {
		t.Fatal("AwaitReady: want an error for a container with no instance label")
	}
}

// TestAwaitPluginLoadSeesThisBoot asserts the plugin count line of the current boot counts.
func TestAwaitPluginLoadSeesThisBoot(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) {
		c.Stdout("[Info   :   BepInEx] 1 plugin to load\n")
	}
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	if !streams.AwaitPluginLoad(t.Context(), id, 5*time.Second) {
		t.Error("AwaitPluginLoad = false, want true: this boot printed a plugin count")
	}
}

// TestAwaitPluginLoadIgnoresAnEarlierBoot asserts a container's log survives every restart, so
// a plugin line from an earlier boot must not satisfy the check for this one.
func TestAwaitPluginLoadIgnoresAnEarlierBoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := runtime.NewFake()
		id := labelledContainer(t, rt)
		startContainer(t, rt, id)
		rt.Get(id).Stdout("[Info   :   BepInEx] 1 plugin to load\n")

		time.Sleep(2 * bootStartMargin)
		startContainer(t, rt, id)
		rt.Get(id).Stdout("Game server connected\n")
		streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
		defer streams.Shutdown()

		if streams.AwaitPluginLoad(t.Context(), id, 50*time.Millisecond) {
			t.Error("AwaitPluginLoad = true, but the only plugin line belongs to an earlier boot")
		}
	})
}

// TestAwaitReadyIgnoresAnEarlierBoot asserts a ready line from before a restart of the same
// container does not confirm the new boot.
func TestAwaitReadyIgnoresAnEarlierBoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := runtime.NewFake()
		id := labelledContainer(t, rt)
		startContainer(t, rt, id)
		rt.Get(id).Stdout("Game server connected\n")

		time.Sleep(2 * bootStartMargin)
		startContainer(t, rt, id)
		rt.Get(id).Stdout("Zonesystem Start 1\n")
		streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
		defer streams.Shutdown()

		confirmed, err := streams.AwaitReady(t.Context(), id, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if confirmed {
			t.Error("confirmed = true, but the only ready line belongs to an earlier boot")
		}
	})
}

// TestCheckReadyIsASingleLook asserts the cold check answers from the log as it stands and
// errors for an exited container.
func TestCheckReadyIsASingleLook(t *testing.T) {
	rt := runtime.NewFake()
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	logs := NewContainerLogs(rt, DefaultPatterns)

	if ready, err := logs.CheckReady(t.Context(), id); err != nil || ready {
		t.Fatalf("CheckReady before the line = %v, %v, want false", ready, err)
	}
	rt.Get(id).Stdout("Game server connected\n")
	if ready, err := logs.CheckReady(t.Context(), id); err != nil || !ready {
		t.Fatalf("CheckReady after the line = %v, %v, want true", ready, err)
	}
	rt.Get(id).Exit(1)
	if _, err := logs.CheckReady(t.Context(), id); err == nil {
		t.Fatal("CheckReady: want an error for an exited container")
	}
}

// TestSyncBootStartsAFreshRecordForANewBoot asserts a reader that finds its container on a new
// boot ends the earlier boot's record, so nothing the earlier boot logged counts for this one.
func TestSyncBootStartsAFreshRecordForANewBoot(t *testing.T) {
	r := newReader(DefaultPatterns)
	first := time.Now()
	r.syncBoot(first)
	earlier := r.latch()
	earlier.note(EventReady)

	r.syncBoot(first)
	if r.latch() != earlier {
		t.Fatal("the record was replaced although the boot did not change")
	}
	r.syncBoot(first.Add(time.Minute))

	select {
	case <-earlier.exited:
	default:
		t.Error("the earlier boot's record was not ended, so its waiters would never look again")
	}
	select {
	case <-r.latch().channel(EventReady):
		t.Error("the new boot inherited the ready line of the earlier one")
	default:
	}
}

// TestAwaitReadyOutlivesAStoppedReader asserts a wait goes on when the reader is stopped under
// it, as a reconcile pass can do, and still hears the line.
func TestAwaitReadyOutlivesAStoppedReader(t *testing.T) {
	rt := runtime.NewFake()
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	defer streams.Shutdown()

	go func() {
		time.Sleep(100 * time.Millisecond)
		streams.Close("inst-a")
		time.Sleep(200 * time.Millisecond)
		rt.Get(id).Stdout("Game server connected\n")
	}()
	confirmed, err := streams.AwaitReady(t.Context(), id, 30*time.Second)
	if err != nil || !confirmed {
		t.Fatalf("AwaitReady = %v, %v, want confirmed after the reader was stopped and started again", confirmed, err)
	}
}

// TestAwaitReadyAfterShutdownReadsTheLogDirectly asserts a wait started after the streams were
// shut down still answers from the log, and starts no reader.
func TestAwaitReadyAfterShutdownReadsTheLogDirectly(t *testing.T) {
	rt := runtime.NewFake()
	rt.OnStart = func(c *runtime.FakeContainer) { c.Stdout("Game server connected\n") }
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	streams := NewStreams(rt, DefaultPatterns, StreamHooks{})
	streams.Shutdown()

	confirmed, err := streams.AwaitReady(t.Context(), id, 5*time.Second)
	if err != nil || !confirmed {
		t.Fatalf("AwaitReady = %v, %v, want confirmed from the log", confirmed, err)
	}
	if r := streams.Reader("inst-a"); r != nil && r.loop.running() != "" {
		t.Error("a reader was started after Shutdown")
	}
}
