package instance

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// framed builds a Docker-framed log stream. Each write becomes one frame, which is what lets
// a test place a frame boundary exactly where it wants one.
type framed struct {
	buf bytes.Buffer
}

func (f *framed) out(s string) { f.write(stdcopy.Stdout, s) }
func (f *framed) err(s string) { f.write(stdcopy.Stderr, s) }

func (f *framed) write(stream stdcopy.StdType, s string) {
	if _, err := stdcopy.NewStdWriter(&f.buf, stream).Write([]byte(s)); err != nil {
		panic(err)
	}
}

func collect(t *testing.T, f *framed) []Line {
	t.Helper()
	var lines []Line
	if err := DemuxLines(bytes.NewReader(f.buf.Bytes()), func(l Line) { lines = append(lines, l) }); err != nil {
		t.Fatalf("DemuxLines: %v", err)
	}
	return lines
}

// TestFrameBoundaryMidLine is E5, and the boundary is placed mid-line deliberately: short
// lines almost never straddle a frame, so the bug that halves console lines in production is
// invisible in a test that does not force it.
func TestFrameBoundaryMidLine(t *testing.T) {
	f := &framed{}
	f.out("World save writ")
	f.out("ing finished\n")

	lines := collect(t, f)
	if len(lines) != 1 || lines[0].Text != "World save writing finished" {
		t.Fatalf("got %d lines %v, want one whole line", len(lines), lines)
	}
	// The reassembled line must still satisfy the pattern set: a half-line matches nothing,
	// which is how this bug reaches a backup that never quiesces.
	if ev, ok := DefaultPatterns.Match(lines[0].Text); !ok || ev.Kind != EventSaveComplete {
		t.Errorf("the reassembled line did not match the save-complete pattern")
	}
}

// TestOneFrameManyLines is the other half of E5: a frame is not a line in either direction.
func TestOneFrameManyLines(t *testing.T) {
	f := &framed{}
	f.out("Chainloader started\n1 plugin to load\nLoading [Jotunn 2.29.2]\n")

	lines := collect(t, f)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(lines), lines)
	}
}

// TestStreamsStayTagged is 02 §4.5's measurement: with Tty false the streams stay separate,
// both are forwarded, and neither is deduplicated against the other.
func TestStreamsStayTagged(t *testing.T) {
	f := &framed{}
	f.out("on stdout\n")
	f.err("on stderr\n")

	lines := collect(t, f)
	if len(lines) != 2 || lines[0].Stream != StreamStdout || lines[1].Stream != StreamStderr {
		t.Fatalf("streams lost their tags: %+v", lines)
	}
}

// TestInterleavedStreamsReassembleSeparately is why reassembly is per stream id: a stderr
// frame arriving between two halves of a stdout line must not be spliced into it.
func TestInterleavedStreamsReassembleSeparately(t *testing.T) {
	f := &framed{}
	f.out("Game server ")
	f.err("a warning\n")
	f.out("connected\n")

	lines := collect(t, f)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	if lines[0].Text != "a warning" || lines[1].Text != "Game server connected" {
		t.Errorf("streams were spliced together: %+v", lines)
	}
}

func TestLongLineIsTruncatedNotBuffered(t *testing.T) {
	f := &framed{}
	f.out(strings.Repeat("x", MaxLineBytes+4096) + "\ndone\n")

	lines := collect(t, f)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !strings.HasSuffix(lines[0].Text, TruncationMarker) {
		t.Errorf("the over-long line carries no truncation marker")
	}
	if n := len(lines[0].Text); n != MaxLineBytes+len(TruncationMarker) {
		t.Errorf("truncated line is %d bytes, want %d", n, MaxLineBytes+len(TruncationMarker))
	}
	// The discarded tail must not leak into the next line.
	if lines[1].Text != "done" {
		t.Errorf("line after the truncated one = %q, want \"done\"", lines[1].Text)
	}
}

// TestDockerTimestampIsSplitOff covers 14 §4.1: the timestamp is Docker's, taken off the
// wire rather than read from the reader's clock.
func TestDockerTimestampIsSplitOff(t *testing.T) {
	f := &framed{}
	f.out("2026-08-20T08:17:58.123456789Z Game server connected\n")
	f.out("no timestamp here\n")

	lines := collect(t, f)
	want := time.Date(2026, 8, 20, 8, 17, 58, 123456789, time.UTC)
	if !lines[0].TS.Equal(want) || lines[0].Text != "Game server connected" {
		t.Errorf("timestamp not split off: %+v", lines[0])
	}
	if !lines[1].TS.IsZero() || lines[1].Text != "no timestamp here" {
		t.Errorf("a line without a timestamp was mangled: %+v", lines[1])
	}
}

// TestStreamEndingMidLineStillEmitsIt: a server killed mid-write owes the console its last
// half-line, which is often the interesting one.
func TestStreamEndingMidLineStillEmitsIt(t *testing.T) {
	f := &framed{}
	f.out("Segmentation fault (no newline)")

	lines := collect(t, f)
	if len(lines) != 1 || lines[0].Text != "Segmentation fault (no newline)" {
		t.Fatalf("trailing partial line lost: %+v", lines)
	}
}
