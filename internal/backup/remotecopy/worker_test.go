package remotecopy

import (
	"testing"
	"time"
)

// TestRemoteBackoffStaysWithinJitterAndHourlyCap asserts the retry delay doubles with jitter and
// never exceeds an hour.
func TestRemoteBackoffStaysWithinJitterAndHourlyCap(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		min     time.Duration
		max     time.Duration
	}{
		{attempt: 1, min: time.Minute, max: 72 * time.Second},
		{attempt: 2, min: 2 * time.Minute, max: 144 * time.Second},
		{attempt: 20, min: time.Hour, max: time.Hour},
	} {
		for range 25 {
			got := backoff(tc.attempt)
			if got < tc.min || got > tc.max {
				t.Errorf("backoff(%d) = %s, want within [%s, %s]", tc.attempt, got, tc.min, tc.max)
			}
		}
	}
}
