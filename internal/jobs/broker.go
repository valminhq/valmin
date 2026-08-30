package jobs

import "sync"

// Event is what publishes to job.{id} (04 §4's job message) — in memory, immediately,
// ahead of the WebSocket hub that will carry it further (WP-21). LogLine is empty for a
// pure progress event.
type Event struct {
	JobID    string
	Status   string
	Progress int
	Message  string
	LogLine  string
}

// broker fans a job's progress and log lines out to whoever is watching it. It is the seam
// WP-21's hub subscribes through for the job.{id} topic — not that topic's backpressure
// policy (ADR-039), which is the hub's own.
type broker struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: make(map[string]map[chan Event]struct{})} }

// Subscribe returns a channel of jobID's events and a cancel func that must be called to
// stop receiving and release the channel.
func (b *broker) Subscribe(jobID string) (events <-chan Event, cancel func()) {
	ch := make(chan Event, 64)

	b.mu.Lock()
	if b.subs[jobID] == nil {
		b.subs[jobID] = make(map[chan Event]struct{})
	}
	b.subs[jobID][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[jobID], ch)
			if len(b.subs[jobID]) == 0 {
				delete(b.subs, jobID)
			}
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// publish never blocks the caller (C21) — the worker goroutine that calls this must never
// stall because a subscriber stopped reading. A full channel drops the event; the row
// written at Finish is always the authoritative source, per 12 §7's subscribe-then-fetch
// rule.
func (b *broker) publish(jobID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[jobID] {
		select {
		case ch <- ev:
		default:
		}
	}
}
