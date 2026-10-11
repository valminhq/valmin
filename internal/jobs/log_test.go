package jobs

import (
	"strings"
	"testing"
)

// TestCappedLogKeepsTheNewestLines: the tail is where a failure is explained, so eviction
// drops from the front and the marker says something was dropped.
func TestCappedLogKeepsTheNewestLines(t *testing.T) {
	b := newCappedLog(12)
	for _, line := range []string{"first", "second", "third"} {
		b.Append(line)
	}
	got := b.String()
	if strings.Contains(got, "first") {
		t.Errorf("log = %q, still holds the oldest line past the cap", got)
	}
	if !strings.HasSuffix(got, "third\n") {
		t.Errorf("log = %q, want it to end with the newest line", got)
	}
	if len(got) > 12 {
		t.Errorf("log is %d bytes, over the 12-byte cap", len(got))
	}
}

// TestCappedLogExactlyAtTheCapIsNotTruncated pins the boundary: a log that fits, newline
// included, is kept whole and carries no marker claiming lines were lost.
func TestCappedLogExactlyAtTheCapIsNotTruncated(t *testing.T) {
	b := newCappedLog(12)
	b.Append("abcde") // 6 bytes with its newline
	b.Append("fghij") // 12 in total

	if got, want := b.String(), "abcde\nfghij\n"; got != want {
		t.Errorf("log = %q, want %q whole and unmarked", got, want)
	}
}

// TestCappedLogKeepsAnOversizedLastLine: one line longer than the cap is the only evidence
// left, so it is kept and cut to the cap rather than evicted to nothing.
func TestCappedLogKeepsAnOversizedLastLine(t *testing.T) {
	b := newCappedLog(8)
	b.Append("short")
	b.Append(strings.Repeat("x", 20))

	got := b.String()
	if len(got) != 8 {
		t.Errorf("log = %q (%d bytes), want exactly the 8-byte cap", got, len(got))
	}
	if !strings.HasSuffix(got, "xxx\n") {
		t.Errorf("log = %q, want the tail of the oversized line", got)
	}
}
