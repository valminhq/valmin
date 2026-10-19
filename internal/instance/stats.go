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

// Sample is one stats message (04 §4). The pointer fields are the honest ones: a nil is
// "the panel does not know", which is a different statement from zero and renders
// differently.
type Sample struct {
	TS time.Time
	// CPUPct is nil on the first sample of a container: Docker reports cumulative
	// CPU-nanoseconds, so a percentage needs two samples and the first has no predecessor (E10).
	CPUPct   *float64
	MemBytes uint64
	MemLimit uint64
	// MemPct is computed from the container's limit, never the host's total (E11) — an
	// unlimited container would otherwise report a meaningless fraction of host RAM. It is
	// nil when the runtime reports no limit at all.
	MemPct *float64
	// Players is the count derived from the log, and nil whenever the panel cannot say — a
	// container whose log it has not read, or a session whose evidence broke (E7, ADR-154).
	Players *int
}

// Sampler polls one container's resource usage. One goroutine per running container
// (14 §4.3), publishing every SampleInterval to whoever is subscribed.
type Sampler struct {
	mu   sync.Mutex
	subs subscribers[Sample]
	// prev is the previous raw sample, which is the whole reason this type holds state: the
	// CPU percentage is a delta and belongs to the caller that holds both readings
	// (internal/runtime deliberately derives nothing).
	prev *runtime.Stats
	// latest is the last published Sample, for the one-shot read of 04 §3.
	latest *Sample
	loop   loop
	// players reads the log reader's derived count. The sampler owns no parsing of its own:
	// the daemon matches every line exactly once, in the reader (F2).
	players func() *int
}

func newSampler() *Sampler { return &Sampler{} }

// Subscribe calls fn with each sample from now on, until cancel returns. fn runs on the
// sampling loop and must not block.
func (s *Sampler) Subscribe(fn func(Sample)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.subs.add(fn)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.subs.remove(id)
	}
}

// sample converts one raw reading into a Sample, against the previous one.
func (s *Sampler) sample(raw runtime.Stats, now time.Time) Sample {
	s.mu.Lock()
	prev := s.prev
	s.prev = &raw
	s.mu.Unlock()

	out := Sample{TS: now, MemBytes: raw.MemBytes, MemLimit: raw.MemLimit}
	if s.players != nil {
		out.Players = s.players()
	}
	if raw.MemLimit > 0 {
		pct := float64(raw.MemBytes) / float64(raw.MemLimit) * 100
		out.MemPct = &pct
	}
	if pct, ok := cpuPercent(prev, raw); ok {
		out.CPUPct = &pct
	}

	s.mu.Lock()
	s.latest = &out
	s.mu.Unlock()
	return out
}

// cpuPercent is 14 §4.3's arithmetic: the ratio of the two cumulative counters' differences,
// scaled by online CPUs, as `docker stats` reports it.
//
// It distinguishes unknown from idle. Unknown is no predecessor, a system clock that did not
// advance, or a counter that went backwards across a container restart. Idle is a CPU delta of
// exactly zero over a clock that did advance, which is a real 0% and must not read as unknown:
// a quiet server would otherwise graph as permanently blank (E10).
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
	s.subs.send(sm)
}

// subscribers is a set of callbacks, guarded by its owner's lock. The owner calls them on its
// own loop, so a subscriber that blocks stalls the source for everyone (C21).
type subscribers[T any] struct {
	next int
	fns  map[int]func(T)
}

func (s *subscribers[T]) add(fn func(T)) int {
	if s.fns == nil {
		s.fns = make(map[int]func(T))
	}
	s.next++
	s.fns[s.next] = fn
	return s.next
}

func (s *subscribers[T]) remove(id int) { delete(s.fns, id) }

func (s *subscribers[T]) send(v T) {
	for _, fn := range s.fns {
		fn(v)
	}
}

func (s *Sampler) run(ctx context.Context, rt runtime.Runtime, instanceID, containerID string) {
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
			if containerGone(ctx, rt, containerID) {
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

func (s *Sampler) halt() {
	s.loop.halt()
	// A stopped server has no current sample; the last one would read as live.
	s.mu.Lock()
	s.prev, s.latest = nil, nil
	s.mu.Unlock()
}

// start begins sampling containerID, and is a no-op if that is already happening. A
// different container resets the delta: the counters are that container's, and carrying one
// container's reading into another's would produce a first percentage that is pure fiction.
func (s *Sampler) start(rt runtime.Runtime, instanceID, containerID string) {
	if s.loop.running() == containerID {
		return
	}
	// Halting resets the delta and the cached sample with it: the counters are that
	// container's, and answering a read with the previous one's figures would be fiction.
	s.halt()
	s.loop.start(containerID, func(ctx context.Context) { s.run(ctx, rt, instanceID, containerID) })
}

// Latest returns the most recent sample, and false if nothing has been sampled yet. It exists so
// a one-shot read can answer with a real CPU percentage: that number is a delta between two
// samples, so a fresh reading of its own could only report null (E10).
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return Sample{}, false
	}
	return *s.latest, true
}
