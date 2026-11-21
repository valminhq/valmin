package instance

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// connectionsLine is the server's periodic status line as the game writes it.
const connectionsLine = `11/13/2026 19:57:00:  Connections 0 ZDOS:82  sent:0 recv:0`

// TestPulseIsTheBootAndTheLastConnectionsLineOfIt asserts the reader reports when its boot began
// and the receive time of the newest periodic status line of that boot, and nothing else counts.
func TestPulseIsTheBootAndTheLastConnectionsLineOfIt(t *testing.T) {
	boot := time.Date(2026, 11, 13, 19, 46, 49, 0, time.UTC)
	r := newReader(DefaultPatterns)

	if p := r.Pulse(); !p.Boot.IsZero() || !p.Beat.IsZero() {
		t.Fatalf("Pulse = %+v before the reader looked, want zero", p)
	}
	r.bootAt = boot
	r.syncBoot(boot)
	if p := r.Pulse(); !p.Boot.Equal(boot) || !p.Beat.IsZero() {
		t.Fatalf("Pulse = %+v with no status line, want boot %v and no beat", p, boot)
	}
	r.append(Line{Stream: StreamStdout, TS: boot.Add(time.Second), Text: "Game server connected"})
	if p := r.Pulse(); !p.Beat.IsZero() {
		t.Fatal("Pulse counted a line that is not the periodic status line")
	}

	first, second := boot.Add(10*time.Minute), boot.Add(20*time.Minute)
	r.append(Line{Stream: StreamStdout, TS: first, Text: connectionsLine})
	if p := r.Pulse(); !p.Beat.Equal(first) {
		t.Fatalf("beat = %v, want %v", p.Beat, first)
	}
	r.append(Line{Stream: StreamStdout, TS: second, Text: connectionsLine})
	if p := r.Pulse(); !p.Beat.Equal(second) {
		t.Fatalf("beat = %v after a second line, want %v", p.Beat, second)
	}

	// A line stamped before this boot belongs to an earlier one and moves nothing.
	r.append(Line{Stream: StreamStdout, TS: boot.Add(-time.Minute), Text: connectionsLine})
	if p := r.Pulse(); !p.Beat.Equal(second) {
		t.Errorf("beat = %v after an earlier boot's line, want %v", p.Beat, second)
	}

	// A new boot starts without one, and reports its own start.
	next := boot.Add(time.Hour)
	r.bootAt = next
	r.syncBoot(next)
	if p := r.Pulse(); !p.Boot.Equal(next) || !p.Beat.IsZero() {
		t.Errorf("Pulse = %+v after a new boot, want boot %v and no beat", p, next)
	}
}

// TestPulseSaysWhenTheLogHasBeenReadable asserts the reader reports since when the log stream it
// follows has been open, and nothing while none is, so a silence that began while the panel could
// not read the log is not counted against the server.
func TestPulseSaysWhenTheLogHasBeenReadable(t *testing.T) {
	boot := time.Date(2026, 11, 13, 19, 46, 49, 0, time.UTC)
	r := newReader(DefaultPatterns)
	r.bootAt = boot
	r.syncBoot(boot)

	if p := r.Pulse(); !p.Open.IsZero() {
		t.Fatalf("Open = %v before any stream was opened, want zero", p.Open)
	}
	opened := boot.Add(time.Minute)
	r.streamOpened(opened)
	if p := r.Pulse(); !p.Open.Equal(opened) {
		t.Fatalf("Open = %v, want %v", p.Open, opened)
	}
	r.streamClosed()
	if p := r.Pulse(); !p.Open.IsZero() {
		t.Errorf("Open = %v after the stream ended, want zero", p.Open)
	}

	r.streamOpened(opened)
	next := boot.Add(time.Hour)
	r.bootAt = next
	r.syncBoot(next)
	if p := r.Pulse(); !p.Open.IsZero() {
		t.Errorf("Open = %v after a new boot, want zero until its stream opens", p.Open)
	}
}

// TestSawStatusLineAfterAsksTheLogItself asserts a status line the follow stream may have missed
// is found by reading the log once, and only when it is newer than the instant asked about.
func TestSawStatusLineAfterAsksTheLogItself(t *testing.T) {
	rt := runtime.NewFake()
	id := labelledContainer(t, rt)
	startContainer(t, rt, id)
	logs := NewContainerLogs(rt, DefaultPatterns)

	saw := func(after time.Time) bool {
		t.Helper()
		got, err := logs.SawStatusLineAfter(t.Context(), id, after)
		if err != nil {
			t.Fatalf("SawStatusLineAfter: %v", err)
		}
		return got
	}
	if saw(time.Now().Add(-time.Hour)) {
		t.Fatal("found a status line in a log that has none")
	}
	rt.Get(id).Stdout("Game server connected\n")
	if saw(time.Now().Add(-time.Hour)) {
		t.Error("counted a line that is not the periodic status line")
	}
	rt.Get(id).Stdout(connectionsLine + "\n")
	if !saw(time.Now().Add(-time.Hour)) {
		t.Error("missed a status line newer than the instant asked about")
	}
	if saw(time.Now().Add(time.Hour)) {
		t.Error("found a status line older than the instant asked about")
	}
}
