package instance

import (
	"math"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// TestFirstSampleHasNoCPUPercentage is E10, and the distinction it draws is the whole point:
// null means "not known yet", 0 means "idle". A dashboard that opens at 0% for a server
// pegged at 300% teaches operators to distrust the graph, and they are right to.
func TestFirstSampleHasNoCPUPercentage(t *testing.T) {
	s := newSampler()
	raw := runtime.Stats{CPUNanos: 1e9, SystemNanos: 100e9, OnlineCPUs: 8, MemBytes: 1 << 30, MemLimit: 4 << 30}

	first := s.sample(raw, time.Now())
	if first.CPUPct != nil {
		t.Errorf("the first sample carries cpu_pct = %v, want nil", *first.CPUPct)
	}

	raw.CPUNanos, raw.SystemNanos = 5e9, 200e9
	second := s.sample(raw, time.Now())
	if second.CPUPct == nil {
		t.Fatal("the second sample carries no cpu_pct, but it has a predecessor")
	}
	// 4e9 / 100e9 × 8 CPUs × 100 = 32%
	if math.Abs(*second.CPUPct-32) > 1e-9 {
		t.Errorf("cpu_pct = %v, want 32", *second.CPUPct)
	}
}

// TestCPUIsADeltaNotAReading: the counters are cumulative, so a container sitting idle
// between two samples reads 0% however large its lifetime total is — and 0 is the answer,
// not nil. An idle server is the normal case for a friend-group panel, and reporting it as
// "unknown" leaves the graph permanently blank. Measured against a real container: the stub
// burns exactly zero additional nanoseconds between samples.
func TestCPUIsADeltaNotAReading(t *testing.T) {
	s := newSampler()
	huge := runtime.Stats{CPUNanos: 900e9, SystemNanos: 1000e9, OnlineCPUs: 4}
	s.sample(huge, time.Now())

	idle := huge
	idle.SystemNanos += 10e9 // ten seconds of wall clock, no CPU burned
	got := s.sample(idle, time.Now())
	if got.CPUPct == nil {
		t.Fatal("an idle container reported nil, but 0% is knowable and true")
	}
	if *got.CPUPct != 0 {
		t.Errorf("an idle container reported %v%%, want 0", *got.CPUPct)
	}
}

// TestStalledSystemClockIsUnknown: with no elapsed system time there is nothing to divide
// by, which is genuinely unknown rather than idle.
func TestStalledSystemClockIsUnknown(t *testing.T) {
	s := newSampler()
	same := runtime.Stats{CPUNanos: 900e9, SystemNanos: 1000e9, OnlineCPUs: 4}
	s.sample(same, time.Now())
	if got := s.sample(same, time.Now()); got.CPUPct != nil {
		t.Errorf("a stalled system clock produced cpu_pct = %v, want nil", *got.CPUPct)
	}
}

// TestCountersGoingBackwardsAreNotAPercentage: a restarted container resets its counters,
// and a negative delta rendered as a percentage is worse than a gap in the graph.
func TestCountersGoingBackwardsAreNotAPercentage(t *testing.T) {
	s := newSampler()
	s.sample(runtime.Stats{CPUNanos: 900e9, SystemNanos: 1000e9, OnlineCPUs: 4}, time.Now())
	got := s.sample(runtime.Stats{CPUNanos: 1e9, SystemNanos: 2e9, OnlineCPUs: 4}, time.Now())
	if got.CPUPct != nil {
		t.Errorf("reset counters produced cpu_pct = %v, want nil", *got.CPUPct)
	}
}

// TestMemoryPercentageIsAgainstTheContainersLimit is E11. The runtime adapter has already
// done Q24's inactive_file subtraction; what is decided here is the denominator.
func TestMemoryPercentageIsAgainstTheContainersLimit(t *testing.T) {
	s := newSampler()
	got := s.sample(runtime.Stats{MemBytes: 1 << 30, MemLimit: 4 << 30}, time.Now())
	if got.MemPct == nil || math.Abs(*got.MemPct-25) > 1e-9 {
		t.Errorf("mem_pct = %v, want 25", got.MemPct)
	}
	if got.MemBytes != 1<<30 || got.MemLimit != 4<<30 {
		t.Errorf("the raw numbers were altered: %d / %d", got.MemBytes, got.MemLimit)
	}

	// A runtime that reports no limit gets no percentage rather than a division by zero.
	if none := s.sample(runtime.Stats{MemBytes: 1 << 30}, time.Now()); none.MemPct != nil {
		t.Errorf("mem_pct = %v with no limit reported, want nil", *none.MemPct)
	}
}

// TestPlayersIsAlwaysNil is E7 and Q7. The field exists so the wire carries `"players":
// null` rather than omitting it, and it is never set by anything.
func TestPlayersIsAlwaysNil(t *testing.T) {
	s := newSampler()
	for range 5 {
		got := s.sample(runtime.Stats{CPUNanos: 1, SystemNanos: 1, MemBytes: 1}, time.Now())
		if got.Players != nil {
			t.Fatalf("players = %d, want nil: Q7 is post-1.0 (E7)", *got.Players)
		}
	}
}

func TestSamplerPublishesToSubscribers(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	fake.Get(id).Stats = runtime.Stats{
		CPUNanos: 1e9, SystemNanos: 10e9, OnlineCPUs: 2, MemBytes: 1 << 20, MemLimit: 1 << 30,
	}

	streams := NewStreams(fake)
	defer streams.Shutdown()
	streams.Open("inst-a", id)

	sampler := streams.Sampler("inst-a")
	if sampler == nil {
		t.Fatal("Open started no sampler")
	}
	samples, cancel := sampler.Subscribe()
	defer cancel()

	select {
	case got := <-samples:
		if got.MemBytes != 1<<20 {
			t.Errorf("mem_bytes = %d", got.MemBytes)
		}
	case <-time.After(SampleInterval * 3):
		t.Fatal("no sample arrived")
	}
}

// TestSamplerStopsWithTheContainer is 14 §8: a stopped server has no resource usage worth
// graphing. The ring buffer, by contrast, stays — see
// TestLogsOpenReadsAndCloseKeepsTheBuffer.
func TestSamplerStopsWithTheContainer(t *testing.T) {
	fake := runtime.NewFake()
	id, err := fake.Create(t.Context(), &runtime.ContainerSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	streams := NewStreams(fake)
	defer streams.Shutdown()
	streams.Open("inst-a", id)
	sampler := streams.Sampler("inst-a")
	waitFor(t, func() bool { return sampler.sampling() == id })

	streams.Close("inst-a")
	if sampler.sampling() != "" {
		t.Error("the sampler is still running after the instance stopped")
	}
}
