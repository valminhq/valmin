package ratelimit

import (
	"testing"
	"time"
)

// TestLimiterRefusesAndRecovers pins the arithmetic behind Retry-After: 429 always carries
// one (11 §7), and it has to be long enough that obeying it actually succeeds.
func TestLimiterRefusesAndRecovers(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 3)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if allowed, _ := l.Allow("1.2.3.4"); !allowed {
			t.Fatalf("request %d refused inside the burst", i+1)
		}
	}
	allowed, retry := l.Allow("1.2.3.4")
	if allowed {
		t.Fatal("fourth request allowed; the burst is 3")
	}
	if retry <= 0 {
		t.Fatal("no Retry-After on a refusal")
	}

	if other, _ := l.Allow("5.6.7.8"); !other {
		t.Error("a different key was refused; buckets are per key")
	}

	now = now.Add(retry)
	if allowed, _ := l.Allow("1.2.3.4"); !allowed {
		t.Errorf("still refused after waiting the advertised %s", retry)
	}
}

// TestLimiterRefillsAtTheConfiguredRate pins the rate, not just recovery: at 60 a minute an
// exhausted bucket earns one token a second. A limiter refilling faster still recovers after
// its own Retry-After, so TestLimiterRefusesAndRecovers alone would not notice a login guard
// that had quietly become ten times as generous.
func TestLimiterRefillsAtTheConfiguredRate(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 1)
	l.now = func() time.Time { return now }

	if allowed, _ := l.Allow("1.2.3.4"); !allowed {
		t.Fatal("first request refused")
	}
	now = now.Add(900 * time.Millisecond)
	allowed, retry := l.Allow("1.2.3.4")
	if allowed {
		t.Fatal("allowed 0.9s after spending the only token; the rate is one a second")
	}
	if retry > 2*time.Second {
		t.Errorf("Retry-After = %s for a token 0.1s away; a caller would wait far longer than needed", retry)
	}
	now = now.Add(100 * time.Millisecond)
	if allowed, _ := l.Allow("1.2.3.4"); !allowed {
		t.Error("refused a full second after the last token was spent")
	}
}

// TestLimiterIdleTimeDoesNotBankPastTheBurst: however long a key stays quiet, it comes back
// to a full burst and no more. Otherwise an hour of silence would buy an hour's worth of
// password guesses in one go.
func TestLimiterIdleTimeDoesNotBankPastTheBurst(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 3)
	l.now = func() time.Time { return now }

	l.Allow("1.2.3.4")
	now = now.Add(time.Hour)
	for i := range 3 {
		if allowed, _ := l.Allow("1.2.3.4"); !allowed {
			t.Fatalf("request %d refused inside the burst after an idle hour", i+1)
		}
	}
	if allowed, _ := l.Allow("1.2.3.4"); allowed {
		t.Error("a fourth request was allowed; idle time banked tokens past the burst of 3")
	}
}

// TestLimiterSweepsFullBucketsBeforeSpentOnes: a full bucket holds nothing a fresh one would
// not, so it is what the sweep drops first. Dropping a drained one instead would hand that
// key a fresh burst.
func TestLimiterSweepsFullBucketsBeforeSpentOnes(t *testing.T) {
	now := time.Now()
	l := NewLimiter(1, time.Hour, 1)
	l.maxKeys = 3
	l.now = func() time.Time { return now }

	l.Allow("spent") // the only token, which takes an hour to come back
	l.buckets["full-a"] = &bucket{tokens: 1, last: now}
	l.buckets["full-b"] = &bucket{tokens: 1, last: now}
	now = now.Add(time.Second)
	l.Allow("newcomer") // the table is at maxKeys, so this sweeps

	if _, ok := l.buckets["spent"]; !ok {
		t.Fatal("the sweep dropped a drained bucket while full ones were there to drop")
	}
	if allowed, _ := l.Allow("spent"); allowed {
		t.Error("the drained key was allowed again after the sweep")
	}
}

// TestLimiterTableStaysBounded: the keys are caller-supplied addresses, so an unbounded
// table would be a memory primitive rather than a control.
func TestLimiterTableStaysBounded(t *testing.T) {
	now := time.Now()
	l := NewLimiter(60, time.Minute, 1)
	l.maxKeys = 8
	l.now = func() time.Time { return now }

	for i := range 100 {
		l.Allow(string(rune('a'+i%26)) + string(rune('a'+i/26)))
		now = now.Add(time.Millisecond)
	}
	if len(l.buckets) > l.maxKeys {
		t.Errorf("table holds %d keys, want at most %d", len(l.buckets), l.maxKeys)
	}
}
