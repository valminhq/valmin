package instance

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// The two stream ids a line can carry. 02 §4.5 measured 30 BepInEx lines on stdout and 0 on
// stderr, and 07 §3 keeps Tty false so the two never merge: both are forwarded, tagged, and
// never deduplicated against each other.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// MaxLineBytes caps one console line. 14 §4.1: a longer line is emitted truncated rather
// than buffered indefinitely, because the alternative is a mod that logs without a newline
// growing the reader's memory without bound.
const MaxLineBytes = 8 << 10

// TruncationMarker is appended to a line cut at MaxLineBytes, so the console shows that
// something was dropped rather than silently losing the tail.
const TruncationMarker = "… [line truncated]"

// Line is one whole log line, as the reader emits it.
type Line struct {
	// Stream is StreamStdout or StreamStderr.
	Stream string
	// TS is Docker's receive time, zero if the stream carried no timestamp.
	//
	// `↯` Docker's, never the reader's clock (14 §4.1). A replayed line carries when the
	// server said it, not when the panel happened to read it — after a reader restart those
	// differ by however long the restart took, and a console whose timestamps jump backwards
	// is the kind of thing nobody reports and everybody distrusts.
	TS time.Time
	// Text is the line without its trailing newline and without Docker's timestamp prefix.
	// The game's own MM/DD/YYYY prefix, where a line carries one, is still present here and
	// is stripped by PatternSet.Match (03 §3.5).
	Text string
}

// DemuxLines reads Docker's multiplexed log stream and calls emit once per whole line, in
// stream order. It returns when the stream ends.
//
// `↯` Frames are not lines (E5). With Tty false the stream is Docker's framing: an 8-byte
// header per frame carrying the stream id and a length, then the payload. A frame boundary
// can fall mid-line and one frame can carry several lines, so the payload is reassembled per
// stream id before anything is emitted. This is the bug that produces a console with
// occasional lines split in half, and it is invisible in testing because short lines almost
// never straddle a frame — which is why the test places a boundary mid-line deliberately.
//
// The frame header itself is stripped by stdcopy, the Docker SDK's own demuxer, rather than
// by a hand-rolled parse of a format this package does not own. It writes each frame's
// payload to the writer for its stream in the order the frames arrive, so interleaving is
// preserved and the line reassembly is per stream.
func DemuxLines(r io.Reader, emit func(Line)) error {
	out := &lineSplitter{stream: StreamStdout, emit: emit}
	errs := &lineSplitter{stream: StreamStderr, emit: emit}

	_, err := stdcopy.StdCopy(out, errs, r)
	// A stream that ends mid-line still owes the caller that line: the server was killed, or
	// the daemon dropped the connection, and the half-line is often the interesting one.
	out.flush()
	errs.flush()
	if err != nil {
		return fmt.Errorf("demux log stream: %w", err)
	}
	return nil
}

// lineSplitter reassembles one stream's payload into whole lines.
type lineSplitter struct {
	stream string
	emit   func(Line)
	buf    []byte
	// cut records that the current line hit MaxLineBytes: the rest of it is discarded up to
	// the next newline rather than buffered.
	cut bool
}

func (w *lineSplitter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.append(p)
			return n, nil
		}
		w.append(p[:i])
		w.flush()
		p = p[i+1:]
	}
}

func (w *lineSplitter) append(b []byte) {
	if w.cut {
		return
	}
	if room := MaxLineBytes - len(w.buf); len(b) > room {
		w.buf = append(w.buf, b[:room]...)
		w.buf = append(w.buf, TruncationMarker...)
		w.cut = true
		return
	}
	w.buf = append(w.buf, b...)
}

func (w *lineSplitter) flush() {
	if len(w.buf) == 0 {
		return
	}
	ts, text := splitTimestamp(strings.TrimSuffix(string(w.buf), "\r"))
	w.emit(Line{Stream: w.stream, TS: ts, Text: text})
	w.buf = w.buf[:0]
	w.cut = false
}

// splitTimestamp separates the RFC3339 prefix Docker writes when LogOptions.Timestamps is
// set. A line without one — anything read from a stream opened without timestamps — is
// returned unchanged with a zero time rather than mangled.
func splitTimestamp(s string) (ts time.Time, text string) {
	i := strings.IndexByte(s, ' ')
	if i <= 0 {
		return time.Time{}, s
	}
	ts, err := time.Parse(time.RFC3339Nano, s[:i])
	if err != nil {
		return time.Time{}, s
	}
	return ts, s[i+1:]
}
