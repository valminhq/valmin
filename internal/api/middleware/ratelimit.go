package middleware

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// Limiter is a per-key token bucket. 11 §7 keeps rate limiting in memory and per process:
// ADR-031 guarantees one daemon per database, so there is no shared store to build.
//
// The keys are caller-supplied — an IP, a username — so the table is bounded and swept
// rather than left to grow.
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

// NewLimiter allows burst requests immediately and then per per period, per key.
func NewLimiter(per int, period time.Duration, burst int) *Limiter {
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

// sweep keeps the table bounded. A full bucket carries no state worth remembering, so it
// is dropped first; if that is not enough the least recently seen key goes.
//
// ponytail: linear scan, bounded by maxKeys. A heap only if maxKeys ever needs to be large.
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

// RateLimit is the unauthenticated per-IP limit of 11 §5.1 row 8. It guards login, /setup
// and invite redemption, which all sit below it.
//
// It runs before the handler that hashes a password, not after: at m=64MiB, ten concurrent
// login attempts is 640 MiB on a box already committed to 4 GiB per instance, so hashing
// first would make the limiter a memory amplifier rather than the control for one (D12).
func RateLimit(l *Limiter) Layer {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := ClientIPFrom(r.Context()).String()
			if ok, retry := l.Allow(key); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
				apierr.Write(w, r, apierr.New(apierr.RateLimited))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
