package api

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/ws"
)

func startedStub(t *testing.T, fake *runtime.Fake) string {
	t.Helper()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestManyTabsShareOneLogStream is 02 §4.5's rule stated as a number: one Docker stream per
// *container*, never one per browser tab. Three admins watching one console is one reader
// and three subscribers, and the only way that claim rots unnoticed is if nobody counts.
func TestManyTabsShareOneLogStream(t *testing.T) {
	fake := runtime.NewFake()
	id := startedStub(t, fake)
	fake.Get(id).Stdout("Game server connected\n")

	streams := instance.NewStreams(fake)
	defer streams.Shutdown()
	reader := streams.Open("inst-a", id)
	waitForRing(t, reader, 1)

	socks := &sockets{streams: streams}
	for tab := range 3 {
		replay, _, cancel := socks.console("inst-a")
		t.Cleanup(cancel)
		// Every tab is served from the ring the one reader filled. A subscription that
		// opened a stream of its own would start empty and fill from Tail — which is how
		// "one per tab" looks from here, before it looks like N containers' worth of
		// Docker connections.
		if len(replay) != 1 {
			t.Fatalf("tab %d replayed %d lines, want the 1 the shared reader holds", tab, len(replay))
		}
	}

	if got := fake.LogsCalls(); got != 1 {
		t.Errorf("the log stream was opened %d times for one container and three tabs", got)
	}
}

// TestConsoleReplayIsDeduplicatedBySequence: the subscription is taken before the ring is
// read, so nothing appended in between is lost — and anything landing in both arrives
// exactly once, which is what seq is for (14 §4.2).
func TestConsoleReplayIsDeduplicatedBySequence(t *testing.T) {
	fake := runtime.NewFake()
	id := startedStub(t, fake)
	fake.Get(id).Stdout("[Info   :   BepInEx] 1 plugin to load\nGame server connected\nplaying\n")

	streams := instance.NewStreams(fake)
	defer streams.Shutdown()
	reader := streams.Open("inst-a", id)
	waitForRing(t, reader, 3)

	socks := &sockets{streams: streams}
	replay, live, cancel := socks.console("inst-a")
	defer cancel()

	if len(replay) != 3 {
		t.Fatalf("replay holds %d messages, want 3: %+v", len(replay), replay)
	}
	// `↯` The pinned startup segment comes first and is not repeated by the ring behind it
	// (G8, 14 §4.2) — those lines are the ones that explain a failed boot.
	seen := map[uint64]bool{}
	for i, m := range replay {
		if seen[m.Seq] {
			t.Errorf("message %d repeats seq %d", i, m.Seq)
		}
		seen[m.Seq] = true
		if i > 0 && m.Seq <= replay[i-1].Seq {
			t.Errorf("replay is out of order at %d: %d after %d", i, m.Seq, replay[i-1].Seq)
		}
	}
	if got := replay[0].Payload.(ws.ConsoleMsg); got.Line != "[Info   :   BepInEx] 1 plugin to load" {
		t.Errorf("first replayed line = %q", got.Line)
	}

	// Nothing already replayed is repeated on the live channel — the filter is the point,
	// since the subscription is deliberately opened before the ring is read.
	select {
	case m := <-live:
		t.Errorf("a replayed message was delivered again: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestConsoleSurvivesTheContainerItWasOpenedBefore: a console opened on a stopped server
// must still show the boot when it starts. The subscription is to the instance, not to
// whichever reader happened to exist at subscribe time.
func TestConsoleSurvivesTheContainerItWasOpenedBefore(t *testing.T) {
	fake := runtime.NewFake()
	streams := instance.NewStreams(fake)
	defer streams.Shutdown()

	socks := &sockets{streams: streams}
	replay, live, cancel := socks.console("inst-a")
	defer cancel()
	if len(replay) != 0 {
		t.Fatalf("a never-run instance replayed %d lines", len(replay))
	}

	id := startedStub(t, fake)
	fake.Get(id).Stdout("[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server\n")
	streams.Open("inst-a", id)

	select {
	case m := <-live:
		got, ok := m.Payload.(ws.ConsoleMsg)
		if !ok {
			t.Fatalf("first live message is %T, want a console line", m.Payload)
		}
		if got.Line != "[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server" {
			t.Errorf("live line = %q", got.Line)
		}
		if m.Seq != 1 {
			t.Errorf("seq = %d, want 1: this is the instance's first line", m.Seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the boot of a container started after the subscribe was never delivered")
	}
}

// TestStatsAreForwardedWithTheirNulls: E7 and E10 have to survive the adapter, or the wire
// carries a confident zero for something the panel does not know.
func TestStatsAreForwardedWithTheirNulls(t *testing.T) {
	fake := runtime.NewFake()
	id := startedStub(t, fake)
	fake.Get(id).Stats = runtime.Stats{
		CPUNanos: 1e9, SystemNanos: 10e9, OnlineCPUs: 2, MemBytes: 1 << 20, MemLimit: 1 << 30,
	}

	streams := instance.NewStreams(fake)
	defer streams.Shutdown()
	streams.Open("inst-a", id)

	socks := &sockets{streams: streams}
	_, live, cancel := socks.stats("inst-a")
	defer cancel()

	select {
	case m := <-live:
		got := m.Payload.(ws.StatsMsg)
		if got.CPUPct != nil {
			t.Errorf("cpu_pct = %v on the first sample, want null (E10)", *got.CPUPct)
		}
		if got.Players != nil {
			t.Errorf("players = %v, want null (E7, Q7 is post-1.0)", *got.Players)
		}
		if got.MemBytes != 1<<20 || got.MemLimit != 1<<30 {
			t.Errorf("memory = %d / %d", got.MemBytes, got.MemLimit)
		}
	case <-time.After(instance.SampleInterval * 4):
		t.Fatal("no sample arrived")
	}
}

func waitForRing(t *testing.T, r *instance.Reader, want int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, recent := r.Ring.Replay(); len(recent) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the ring did not reach %d lines", want)
}
