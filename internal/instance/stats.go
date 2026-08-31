package instance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// SampleInterval is how often a sample is published. Docker's stats stream emits roughly
// once a second; sampling at half that is plenty for a graph and halves the fan-out
// (14 §4.3).
const SampleInterval = 2 * time.Second

// sampleQueue is one stats subscriber's buffer. Stats are lossy like the console (14 §2.1),
// and the sampler never blocks on a subscriber (C21).
const sampleQueue = 64

// Sample is one stats message (04 §4). The pointer fields are the honest ones: a nil is
// "the panel does not know", which is a different statement from zero and renders
// differently.
type Sample struct {
	TS time.Time
	// CPUPct is nil on the first sample of a container.
	//
	// `↯` E10: Docker reports *cumulative* CPU-nanoseconds, so a percentage needs two
	// samples and the first has no predecessor. Sending 0 there would open every dashboard
	// at 0% for a server pegged at 300%, which teaches operators to distrust the graph.
	CPUPct   *float64
	MemBytes uint64
	MemLimit uint64
	// MemPct is computed from the container's limit, never the host's total (E11) — an
	// unlimited container would otherwise report a meaningless fraction of host RAM. It is
	// nil when the runtime reports no limit at all.
	MemPct *float64
	// Players is always nil, and that is the decision, not a gap.
	//
	// `↯` E7: Q7 is deliberately post-1.0 — the join and leave lines are the most
	// version-sensitive thing on the list and 03 §10 expects them to move on 9 September.
	// Sending null is honest; shipping a hardcoded pattern that silently reports 0 players
	// forever is the failure to avoid.
	Players *int
}

// Sampler polls one container's resource usage. One goroutine per running container
// (14 §4.3), publishing every SampleInterval to whoever is subscribed.
type Sampler struct {
	mu   sync.Mutex
	subs map[chan Sample]struct{}
	// prev is the previous raw sample, which is the whole reason this type holds state: the
	// CPU percentage is a delta and belongs to the caller that holds both readings
	// (internal/runtime deliberately derives nothing).
	prev   *runtime.Stats
	stop   context.CancelFunc
	done   chan struct{}
	source string
}

func newSampler() *Sampler { return &Sampler{subs: make(map[chan Sample]struct{})} }

// Subscribe returns a channel of samples and a cancel func that must be called to stop
// receiving.
func (s *Sampler) Subscribe() (samples <-chan Sample, cancel func()) {
	ch := make(chan Sample, sampleQueue)

	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// sample converts one raw reading into a Sample, against the previous one.
func (s *Sampler) sample(raw runtime.Stats, now time.Time) Sample {
	s.mu.Lock()
	prev := s.prev
	s.prev = &raw
	s.mu.Unlock()

	out := Sample{TS: now, MemBytes: raw.MemBytes, MemLimit: raw.MemLimit}
	if raw.MemLimit > 0 {
		pct := float64(raw.MemBytes) / float64(raw.MemLimit) * 100
		out.MemPct = &pct
	}
	if pct, ok := cpuPercent(prev, raw); ok {
		out.CPUPct = &pct
	}
	return out
}

// cpuPercent is 14 §4.3's arithmetic: the ratio of the two cumulative counters' differences,
// scaled by online CPUs, which is what `docker stats` reports.
//
// `↯` It distinguishes *unknown* from *idle*, and the line between them is not where it
// first looks. Unknown (false) is: no predecessor, a system clock that did not advance, or a
// CPU counter that went backwards because the container restarted — a negative delta
// rendered as a percentage is worse than a gap in the graph. Idle is a CPU delta of exactly
// zero over a system clock that *did* advance, and that is a real, knowable 0%. Treating it
// as unknown leaves a quiet server — the normal case for a friend-group panel — with a
// permanently blank graph, which is E10's failure mode wearing the opposite sign. Measured
// 31 Aug 2026: the stub burns literally zero additional nanoseconds between two samples.
func cpuPercent(prev *runtime.Stats, cur runtime.Stats) (float64, bool) {
	if prev == nil || cur.CPUNanos < prev.CPUNanos || cur.SystemNanos <= prev.SystemNanos {
		return 0, false
	}
	cpus := float64(cur.OnlineCPUs)
	if cpus == 0 {
		return 0, false
	}
	cpuDelta := float64(cur.CPUNanos - prev.CPUNanos)
	sysDelta := float64(cur.SystemNanos - prev.SystemNanos)
	return cpuDelta / sysDelta * cpus * 100, true
}

func (s *Sampler) publish(sm Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- sm:
		default:
		}
	}
}

func (s *Sampler) run(ctx context.Context, rt runtime.Runtime, instanceID, containerID string, done chan struct{}) {
	defer func() {
		s.mu.Lock()
		if s.source == containerID {
			s.source = ""
		}
		s.mu.Unlock()
		close(done)
	}()

	ticker := time.NewTicker(SampleInterval)
	defer ticker.Stop()
	for {
		raw, err := rt.Stats(ctx, containerID)
		switch {
		case err == nil:
			s.publish(s.sample(raw, time.Now()))
		case ctx.Err() != nil:
			return
		default:
			// A container that has gone away stops the sampler; anything else is transient
			// and the next tick retries. 14 §8 stops the sampler on `stopped` regardless.
			if c, ierr := rt.Inspect(ctx, containerID); ierr == nil && !c.Running {
				return
			}
			slog.WarnContext(ctx, "stats sample failed, will retry",
				slog.String("instance_id", instanceID),
				slog.String("container_id", containerID), slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sampler) sampling() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source
}

func (s *Sampler) halt() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop, s.done = nil, nil
	s.mu.Unlock()

	if stop == nil {
		return
	}
	stop()
	<-done
}

// start begins sampling containerID, and is a no-op if that is already happening. A
// different container resets the delta: the counters are that container's, and carrying one
// container's reading into another's would produce a first percentage that is pure fiction.
func (s *Sampler) start(rt runtime.Runtime, instanceID, containerID string) {
	if s.sampling() == containerID {
		return
	}
	s.halt()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.stop, s.done, s.source, s.prev = cancel, done, containerID, nil
	s.mu.Unlock()

	go s.run(ctx, rt, instanceID, containerID, done)
}
