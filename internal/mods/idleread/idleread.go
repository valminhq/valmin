// Package idleread bounds a response body by silence rather than total time: a download may
// take as long as it keeps sending.
package idleread

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// Reader cancels its request when the body sends nothing for idle, which ends a read that
// would otherwise block for as long as the server holds the connection open.
type Reader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
	fired atomic.Bool
}

// New wraps r, calling cancel once idle passes without a byte. The caller must Stop it.
func New(r io.Reader, idle time.Duration, cancel context.CancelFunc) *Reader {
	ir := &Reader{r: r, idle: idle}
	ir.timer = time.AfterFunc(idle, func() {
		ir.fired.Store(true)
		cancel()
	})
	return ir
}

// Read reads from the wrapped body and restarts the idle timer on any progress.
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err //nolint:wrapcheck // io.Reader contract: io.EOF must reach the caller unwrapped
}

// Stop releases the idle timer.
func (r *Reader) Stop() { r.timer.Stop() }

// Stalled reports whether the idle timer cancelled the request.
func (r *Reader) Stalled() bool { return r.fired.Load() }
