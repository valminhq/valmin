package instance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// SampleInterval is how often a sample is published while anyone is subscribed. Docker's
// stats stream emits roughly once a second; sampling at half that is plenty for a graph and
// halves the fan-out.
const SampleInterval = 2 * time.Second

// idleSampleInterval is how often a sample is taken while nobody is subscribed, keeping
// Latest recent for the one-shot reads.
const idleSampleInterval = 30 * time.Second

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
	// RxBytesPerSecond and TxBytesPerSecond are network traffic since the previous sample.
	// Like CPUPct they are nil without a predecessor, and after a restart reset the counters.
	RxBytesPerSecond *float64
	TxBytesPerSecond *float64
	// Players is the count derived from the log, and nil whenever the panel cannot say — a
	// container whose log it has not read, or a session whose evidence broke (E7, ADR-154).
	Players *int
}

// Sampler polls one container's resource usage. One goroutine per running container,
// sampling every SampleInterval while anyone is subscribed and every idleSampleInterval
// otherwise.
type Sampler struct {
	mu   sync.Mutex
	subs subscribers[Sample]
	// wake tells a waiting loop that a subscriber arrived.
	wake chan struct{}
	// prev is the previous raw sample, which is the whole reason this type holds state: the
	// CPU percentage is a delta and belongs to the caller that holds both readings
	// (internal/runtime deliberately derives nothing).
	prev *runtime.Stats
	// prevAt is when prev was taken, for the network rates.
	prevAt time.Time
	// latest is the last published Sample, for the one-shot read of 04 §3.
	latest *Sample
	loop   loop
	// players reads the log reader's derived count. The sampler owns no parsing of its own:
	// the daemon matches every line exactly once, in the reader (F2).
	players func() *int
}

func newSampler() *Sampler { return &Sampler{wake: make(chan struct{}, 1)} }

// Subscribe calls fn with each sample from now on, until cancel returns. fn runs on the
// sampling loop and must not block.
func (s *Sampler) Subscribe(fn func(Sample)) (cancel func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.subs.add(fn)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.subs.remove(id)
	}
}

// sample converts one raw reading into a Sample, against the previous one.
func (s *Sampler) sample(raw runtime.Stats, now time.Time) Sample {
	s.mu.Lock()
	prev, prevAt := s.prev, s.prevAt
	s.prev, s.prevAt = &raw, now
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
	if prev != nil {
		seconds := now.Sub(prevAt).Seconds()
		if rate, ok := byteRate(prev.RxBytes, raw.RxBytes, seconds); ok {
			out.RxBytesPerSecond = &rate
		}
		if rate, ok := byteRate(prev.TxBytes, raw.TxBytes, seconds); ok {
			out.TxBytesPerSecond = &rate
		}
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

// byteRate is the per-second rate of a cumulative byte counter between two samples. A counter
// that dropped was reset by a restart, and is unknown rather than a negative rate; an unchanged
// one is a real zero.
func byteRate(prev, cur uint64, seconds float64) (float64, bool) {
	if cur < prev || seconds <= 0 {
		return 0, false
	}
	return float64(cur-prev) / seconds, true
}

func (s *Sampler) publish(sm *Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs.send(*sm)
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

// interval is the wait before the next sample: short while anyone is subscribed.
func (s *Sampler) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs.fns) > 0 {
		return SampleInterval
	}
	return idleSampleInterval
}

// run samples containerID until ctx is done or the container is gone.
func (s *Sampler) run(ctx context.Context, rt runtime.Runtime, instanceID, containerID string) {
	for {
		started := time.Now()
		raw, err := rt.Stats(ctx, containerID)
		switch {
		case err == nil:
			sm := s.sample(raw, time.Now())
			s.publish(&sm)
		case ctx.Err() != nil:
			return
		default:
			// A container that has gone away stops the sampler; anything else is transient
			// and the next sample retries.
			if containerGone(ctx, rt, containerID) {
				return
			}
			slog.WarnContext(ctx, "stats sample failed, will retry",
				slog.String("instance_id", instanceID),
				slog.String("container_id", containerID), slog.Any("error", err))
		}

		if !s.wait(ctx, started) {
			return
		}
	}
}

// wait blocks until interval has passed since last, re-reading the interval whenever a
// subscriber arrives, so a new subscriber shortens an idle wait without bringing the next
// sample closer than SampleInterval. It returns false once ctx is done.
func (s *Sampler) wait(ctx context.Context, last time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.wake:
		case <-time.After(time.Until(last.Add(s.interval()))):
			return true
		}
	}
}

func (s *Sampler) halt() {
	s.loop.halt()
	// A stopped server has no current sample; the last one would read as live.
	s.mu.Lock()
	s.prev, s.prevAt, s.latest = nil, time.Time{}, nil
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

// Latest returns the most recent sample, up to idleSampleInterval old while nobody is
// subscribed, and false if nothing has been sampled yet. A one-shot read answers from it
// because the CPU percentage is a delta between two samples.
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return Sample{}, false
	}
	return *s.latest, true
}
