// Package ratelimit is a keyed token-bucket limiter.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter is a per-key token bucket, kept in memory and per process (11 §7), since ADR-031
// guarantees one daemon per database. Keys are caller-supplied, so the table is bounded and
// swept rather than left to grow.
type Limiter struct {
	rate    float64 // tokens per second
	burst   float64
	maxKeys int

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New allows burst requests immediately and then per per period, per key.
func New(per int, period time.Duration, burst int) *Limiter {
	return &Limiter{
		rate:    float64(per) / period.Seconds(),
		burst:   float64(burst),
		maxKeys: 10000,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Allow spends a token for key. When it returns false, the duration is how long the caller
// must wait, which becomes Retry-After: 429 always carries one (11 §7).
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		l.sweep(now)
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Second
	}
	b.tokens--
	return true, 0
}

// sweep keeps the table bounded. A full bucket carries no state worth remembering and is
// dropped first; if that is not enough the least recently seen key goes. Linear and bounded by
// maxKeys.
func (l *Limiter) sweep(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
			continue
		}
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	if len(l.buckets) >= l.maxKeys && oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}
