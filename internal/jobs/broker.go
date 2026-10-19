package jobs

import "sync"

// Event is what publishes to the job.{id} topic — in memory, immediately, ahead of the
// WebSocket hub that carries it further. LogLine is empty for a pure progress event.
type Event struct {
	JobID    string
	Status   string
	Progress int
	Message  string
	LogLine  string
}

// broker fans a job's progress and log lines out to whoever is watching it. It is the seam
// the hub subscribes through for the job.{id} topic, not that topic's backpressure policy,
// which is the hub's own.
type broker struct {
	mu   sync.Mutex
	next int
	subs map[string]map[int]func(Event)
}

func newBroker() *broker { return &broker{subs: make(map[string]map[int]func(Event))} }

// Subscribe calls fn with each of jobID's events from now on, until cancel returns. fn runs on
// the job's worker and must not block (C21).
func (b *broker) Subscribe(jobID string, fn func(Event)) (cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[jobID] == nil {
		b.subs[jobID] = make(map[int]func(Event))
	}
	b.next++
	id := b.next
	b.subs[jobID][id] = fn
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[jobID], id)
		if len(b.subs[jobID]) == 0 {
			delete(b.subs, jobID)
		}
	}
}

func (b *broker) publish(jobID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, fn := range b.subs[jobID] {
		fn(ev)
	}
}
