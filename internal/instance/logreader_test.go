package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

func line(text string) Line { return Line{Stream: StreamStdout, Text: text} }

func TestRingIsBoundedByLinesAndBytes(t *testing.T) {
	t.Run("lines", func(t *testing.T) {
		r := &Ring{}
		for i := range RingLines + 500 {
			r.Append(line(fmt.Sprintf("line %d", i)))
		}
		_, recent := r.Replay()
		if len(recent) != RingLines {
			t.Errorf("ring holds %d lines, want %d", len(recent), RingLines)
		}
		if recent[0].Seq != 501 {
			t.Errorf("oldest surviving line has seq %d, want 501", recent[0].Seq)
		}
	})

	// A line count alone is not a memory bound when one mod can log a megabyte, which is
	// why the byte bound exists at all (14 §4.2).
	t.Run("bytes", func(t *testing.T) {
		r := &Ring{}
		fat := strings.Repeat("x", MaxLineBytes)
		for range 400 { // 400 × 8 KiB = 3.2 MiB, far inside RingLines
			r.Append(line(fat))
		}
		_, recent := r.Replay()
		if len(recent) >= RingLines {
			t.Fatalf("the line bound was the only one applied: %d lines held", len(recent))
		}
		if total := len(recent) * MaxLineBytes; total > RingBytes+MaxLineBytes {
			t.Errorf("ring holds %d bytes, want at most %d", total, RingBytes)
		}
	})
}

// TestStartupSegmentSurvivesRotation is G8. On a busy server the boot lines are gone within
// minutes and they are the ones that explain a failed boot, so an operator opening the
// console at lunchtime must still be able to reach them.
func TestStartupSegmentSurvivesRotation(t *testing.T) {
	r := &Reader{Ring: &Ring{}, patterns: DefaultPatterns, subs: map[chan Entry]struct{}{}, waits: map[*wait]struct{}{}}
	r.append(line("[Message:   BepInEx] BepInEx 5.4.23.3 - valheim_server"))
	r.append(line("[Info   :   BepInEx] 1 plugin to load"))
	r.append(line("[Info   :   BepInEx] Loading [Jotunn 2.29.2]"))
	// Readiness seals the segment: everything after this is the running server, not the boot.
	r.append(line("08/20/2026 08:17:58: Game server connected"))

	for i := range 5000 {
		r.append(line(fmt.Sprintf("noise %d", i)))
	}

	startup, recent := r.Ring.Replay()
	if len(startup) != 4 {
		t.Fatalf("startup segment holds %d lines, want 4: %+v", len(startup), startup)
	}
	if !strings.Contains(startup[2].Text, "Jotunn") {
		t.Errorf("the mods-loaded line is not in the startup segment: %+v", startup)
	}
	if strings.Contains(recent[0].Text, "BepInEx") {
		t.Errorf("the ring did not rotate; this test proves nothing")
	}
}

// TestStartupSegmentIsCappedWithoutAReadinessLine: a vanilla server logs no chainloader
// sequence and a server that never comes up logs no readiness line, so the seal cannot
// depend on either arriving.
func TestStartupSegmentIsCappedWithoutAReadinessLine(t *testing.T) {
	r := &Ring{}
	for i := range StartupLines * 3 {
		r.Append(line(fmt.Sprintf("line %d", i)))
	}
	startup, _ := r.Replay()
	if len(startup) != StartupLines {
		t.Errorf("startup segment grew to %d lines, want it capped at %d", len(startup), StartupLines)
	}
}

// TestAwaitFailsClosedOnStreamReset is C20, and it is the reason jobs read this channel
// instead of subscribing to the console. If the stream restarted, the panel does not know
// whether the line it is waiting for was written while it was not looking — so no line, no
// archive (02 §4.4).
func TestAwaitFailsClosedOnStreamReset(t *testing.T) {
	r := newReader()

	result := make(chan error, 1)
	go func() {
		_, err := r.Await(t.Context(), EventSaveComplete, r.Ring.Seq())
		result <- err
	}()
	waitFor(t, func() bool { return r.waiting() == 1 })

	r.reset()

	select {
	case err := <-result:
		if !errors.Is(err, ErrStreamReset) {
			t.Errorf("Await returned %v, want ErrStreamReset", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return after a stream reset")
	}
}

// TestAwaitSeesALineThatAlreadyArrived is the race a real stop hits: the caller signals the
// server, the save completes and is read, and only then does the caller register its wait. A
// wait that only ever saw the future would block until its own timeout on a save that
// finished perfectly — and 12 §3.4 would report a failed stop on a clean one.
func TestAwaitSeesALineThatAlreadyArrived(t *testing.T) {
	r := newReader()
	mark := r.Ring.Seq()
	r.append(line("World save writing finished"))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ev, err := r.Await(ctx, EventSaveComplete, mark)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if ev.Kind != EventSaveComplete {
		t.Errorf("Await returned %v", ev.Kind)
	}
}

// TestAwaitIgnoresLinesBeforeTheMark: the same server's previous run also wrote a
// save-complete line, and it must not satisfy this run's wait.
func TestAwaitIgnoresLinesBeforeTheMark(t *testing.T) {
	r := newReader()
	r.append(line("World save writing finished"))
	mark := r.Ring.Seq()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.Await(ctx, EventSaveComplete, mark); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Await returned %v, want a timeout: the line predates the mark", err)
	}
}

func TestAwaitReturnsTheMatchedLine(t *testing.T) {
	r := newReader()
	mark := r.Ring.Seq()

	result := make(chan LogEvent, 1)
	go func() {
		ev, err := r.Await(t.Context(), EventSaveComplete, mark)
		if err != nil {
			t.Error(err)
		}
		result <- ev
	}()
	waitFor(t, func() bool { return r.waiting() == 1 })

	r.append(line("World save writing finishing")) // must not satisfy the wait (B2)
	r.append(line("Saved 21771 ZDOs"))             // nor must a different kind
	select {
	case ev := <-result:
		t.Fatalf("Await returned early on %q", ev.Line)
	case <-time.After(50 * time.Millisecond):
	}

	r.append(line("World save writing finished"))
	select {
	case ev := <-result:
		if ev.Kind != EventSaveComplete {
			t.Errorf("Await returned %v", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not return on the save-complete line")
	}
}

// TestSubscriberSeesResetAndSeq: seq is what lets a subscriber notice its own dropped
// messages without comparing content, and the reset entry is what tells it to clear the view
// rather than splice (14 §4.2).
func TestSubscriberSeesResetAndSeq(t *testing.T) {
	r := newReader()
	entries, cancel := r.Subscribe()
	defer cancel()

	r.append(line("first"))
	r.append(line("second"))
	r.reset()

	var got []Entry
	for range 3 {
		select {
		case e := <-entries:
			got = append(got, e)
		case <-time.After(time.Second):
			t.Fatal("subscriber received nothing")
		}
	}
	if got[0].Seq != 1 || got[1].Seq != 2 {
		t.Errorf("sequence numbers are %d, %d; want 1, 2", got[0].Seq, got[1].Seq)
	}
	if !got[2].Reset {
		t.Errorf("the third message is not a stream.reset: %+v", got[2])
	}
	// Sequence numbers do not reset when the stream does.
	r.append(line("third"))
	if e := <-entries; e.Seq != 3 {
		t.Errorf("seq restarted at %d after a reset", e.Seq)
	}
}

// TestPublishNeverBlocks is C21: one sleeping laptop must not freeze every console. A
// subscriber that stopped reading loses messages and finds out from the gap in Seq.
func TestPublishNeverBlocks(t *testing.T) {
	r := newReader()
	_, cancel := r.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := range subscriberQueue * 3 {
			r.append(line(fmt.Sprintf("line %d", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader blocked on a subscriber that stopped reading")
	}
}

func TestLogsOpenReadsAndCloseKeepsTheBuffer(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{User: containerUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	fake.Get(id).Stdout("Game server connected\nready to play\n")

	streams := NewStreams(fake)
	defer streams.Shutdown()
	r := streams.Open("inst-a", id)
	waitFor(t, func() bool { _, recent := r.Ring.Replay(); return len(recent) == 2 })

	// 14 §8: stopping keeps the ring. The console of a stopped server is the most useful
	// moment it has — it is where the reason it stopped is written.
	streams.Close("inst-a")
	if _, recent := streams.Reader("inst-a").Ring.Replay(); len(recent) != 2 {
		t.Errorf("the ring buffer was discarded when the reader stopped")
	}
}

// TestReaderStopsWhenTheContainerIsGone: a stream that ended because the server exited is
// not a stream to re-open, or the reader emits a reset every couple of seconds until the
// next reconcile pass notices.
func TestReaderStopsWhenTheContainerIsGone(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{User: containerUser})
	if err != nil {
		t.Fatal(err)
	}
	fake.Get(id).Stdout("Game - OnApplicationQuit\nWorld save writing finished\n")
	fake.Get(id).Exit(0)

	streams := NewStreams(fake)
	defer streams.Shutdown()
	r := streams.Open("inst-a", id)
	waitFor(t, func() bool { return r.reading() == "" })

	if _, recent := r.Ring.Replay(); len(recent) != 2 {
		t.Errorf("the exited container's last words were not kept: %+v", recent)
	}
}

func (r *Reader) waiting() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.waits)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
