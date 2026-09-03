//go:build integration

// Proves the log reader against a real daemon: Docker's own multiplexed framing, its own
// timestamps, and a true follow stream. The fake runtime's Logs() is a static snapshot, so
// these are the only tests that exercise the live path — the one where a frame boundary can
// land wherever the daemon puts it (E5).
package instance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
)

func TestLogReaderAgainstARealContainer(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp19-reader", "STUB_MODE=modded"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	streams := instance.NewStreams(d)
	defer streams.Shutdown()
	r := streams.Open("wp19", id)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// The readiness line carries the game's MM/DD/YYYY prefix and arrives on a live stream
	// that also carries Docker's own timestamp. Both have to come off before the match (E4).
	if _, err := r.Await(ctx, instance.EventReady, 0); err != nil {
		t.Fatalf("Await(ready): %v", err)
	}

	startup, recent := r.Ring.Replay()
	if len(recent) == 0 {
		t.Fatal("the ring buffer is empty after readiness")
	}
	if recent[0].TS.IsZero() {
		t.Error("lines carry no Docker timestamp: the stream was opened without Timestamps")
	}
	if recent[0].Stream != instance.StreamStdout {
		t.Errorf("first line is tagged %q", recent[0].Stream)
	}
	if !containsLine(startup, "Loading [Jotunn 2.29.2]") {
		t.Errorf("the mods-loaded line is not in the pinned startup segment: %v", texts(startup))
	}
	if !containsLine(startup, "Game server connected") {
		t.Errorf("readiness should be the last line of the startup segment: %v", texts(startup))
	}

	// The matched-event channel on a live stream, which is what a backup quiesce would
	// block on: SIGINT, then the anchored save-complete literal and nothing before it. The
	// mark is taken before the stop, because on a stub the whole shutdown sequence is read
	// before Stop even returns.
	mark := r.Ring.Seq()
	if err := d.Stop(t.Context(), id, "SIGINT", 30*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ev, err := r.Await(ctx, instance.EventSaveComplete, mark)
	if err != nil {
		t.Fatalf("Await(save_complete): %v", err)
	}
	if !strings.HasSuffix(ev.Line, "World save writing finished") {
		t.Errorf("Await returned on %q", ev.Line)
	}
}

// TestLogReaderNeverSatisfiedByFinishing is B2 on the live path: the stub's no-save-finish
// mode stops after `finishing`, so a wait for the completion line must time out rather than
// be satisfied by the sibling phase. A backup that proceeded here would archive a
// half-written world.
func TestLogReaderNeverSatisfiedByFinishing(t *testing.T) {
	d := dockerRuntime(t)
	id := create(t, d, rawStubSpec(t, "wp19-no-finish", "STUB_MODE=no-save-finish"))
	if err := d.Start(t.Context(), id); err != nil {
		t.Fatalf("start: %v", err)
	}

	streams := instance.NewStreams(d)
	defer streams.Shutdown()
	r := streams.Open("wp19-nf", id)

	ready, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := r.Await(ready, instance.EventReady, 0); err != nil {
		t.Fatalf("Await(ready): %v", err)
	}
	mark := r.Ring.Seq()
	if err := d.Stop(t.Context(), id, "SIGINT", 30*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}

	ctx, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	_, err := r.Await(ctx, instance.EventSaveComplete, mark)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await returned %v; `finishing` must not satisfy a wait for `finished` (B2)", err)
	}
}

func containsLine(entries []instance.Entry, want string) bool {
	for _, e := range entries {
		if strings.Contains(e.Text, want) {
			return true
		}
	}
	return false
}

func texts(entries []instance.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Text
	}
	return out
}
