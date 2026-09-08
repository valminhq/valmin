package instance

import (
	"strconv"
	"sync"
	"time"
)

// PlayerObservation is one change in what the panel can say about an instance's player count.
// A nil Players is "the panel does not know", which is a different statement from zero and is
// the honest answer whenever the evidence broke (E7).
type PlayerObservation struct {
	TS      time.Time
	Players *int
}

// playerCount derives an instance's player count from the log, per the measured shape in
// M5-PLAYER-EVIDENCE.md.
//
// Only lines that state a number set the value. The join and leave events deliberately do not
// increment or decrement: the count line already carries the server's own answer, and it was
// right at every point of the capture including a transport blip that fired a leave line while
// the player stayed connected. The one ending that states nothing — a peer timeout — makes the
// count unknown rather than one lower, because nothing measured says what it should become.
type playerCount struct {
	mu    sync.Mutex
	known bool
	n     int
}

// apply folds one matched line in and reports the new value if it changed. A false second
// result means the caller has nothing to record.
func (p *playerCount) apply(ev LogEvent) (players *int, changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ev.Kind == EventPeerTimeout {
		if !p.known {
			return nil, false
		}
		p.known = false
		return nil, true
	}
	if ev.Kind != EventPlayerCount && ev.Kind != EventConnections {
		return nil, false
	}
	n, err := strconv.Atoi(ev.Groups[1])
	if err != nil {
		return nil, false
	}
	if p.known && p.n == n {
		return nil, false
	}
	p.known, p.n = true, n
	return p.value(), true
}

// invalidate forgets the count, for a stream that restarted or a container that was replaced.
// The panel cannot know what was written while it was not looking (C20, C21).
func (p *playerCount) invalidate() (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.known {
		return false
	}
	p.known = false
	return true
}

func (p *playerCount) current() *int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value()
}

// value copies, so a caller cannot write through the pointer into the tracker.
func (p *playerCount) value() *int {
	if !p.known {
		return nil
	}
	n := p.n
	return &n
}
