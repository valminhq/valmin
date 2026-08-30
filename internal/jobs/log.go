package jobs

// cappedLog is the bounded, tail-truncated in-memory buffer of 12 §7: job logs never
// stream to the database, they accumulate here, fan out live through the broker, and are
// persisted once at terminal status, capped at jobs.log_cap.
type cappedLog struct {
	cap       int
	lines     [][]byte
	size      int
	truncated bool
}

func newCappedLog(capBytes int) *cappedLog { return &cappedLog{cap: capBytes} }

// Append adds one line, evicting the oldest lines once the buffer exceeds its cap.
func (b *cappedLog) Append(line string) {
	ln := []byte(line)
	b.lines = append(b.lines, ln)
	b.size += len(ln) + 1
	for b.size > b.cap && len(b.lines) > 1 {
		b.size -= len(b.lines[0]) + 1
		b.lines = b.lines[1:]
		b.truncated = true
	}
}

// String renders the buffer, marked if lines were evicted, hard-capped at b.cap bytes
// regardless — the marker itself must never be what pushes the value over the limit.
func (b *cappedLog) String() string {
	if len(b.lines) == 0 {
		return ""
	}
	var out []byte
	if b.truncated {
		out = append(out, "… (truncated)\n"...)
	}
	for _, l := range b.lines {
		out = append(out, l...)
		out = append(out, '\n')
	}
	if len(out) > b.cap {
		out = out[len(out)-b.cap:]
	}
	return string(out)
}
