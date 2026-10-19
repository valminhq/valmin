package delivery

import (
	"context"
	"testing"
	"time"
)

// TestRunCancelsAndWaitsForBackgroundAnnouncements asserts shutdown cancels a waiting
// announcement and returns only once it has ended, and that later ones are dropped.
func TestRunCancelsAndWaitsForBackgroundAnnouncements(t *testing.T) {
	t.Parallel()
	n := &Notifier{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		n.Run(ctx)
		close(done)
	}()
	for {
		n.mu.Lock()
		running := n.life != nil
		n.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ended := make(chan struct{})
	n.background(t.Context(), func(ctx context.Context) {
		<-ctx.Done()
		close(ended)
	})
	cancel()
	<-done
	select {
	case <-ended:
	default:
		t.Fatal("Run returned before the announcement ended")
	}

	n.background(t.Context(), func(context.Context) { t.Error("an announcement ran after Run returned") })
}
