package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Handle is what a Runner uses to report itself: progress, log lines, and whether
// cancellation has been requested. It is the only thing a Runner touches — never the
// engine or the store directly, so the writer-pool discipline (C1, C2) is enforced by the
// shape of the API rather than by convention.
type Handle struct {
	engine *Engine
	jobID  string

	mu          sync.Mutex
	progress    int
	message     string
	lastWriteAt time.Time
	lastWriteP  int
	lastWriteM  string
	log         *cappedLog
}

func newHandle(e *Engine, jobID string) *Handle {
	return &Handle{engine: e, jobID: jobID, log: newCappedLog(e.cfg.LogCap)}
}

// Progress records pct/message, publishes immediately to job.{id} (in memory, 12 §7), and
// writes the row at most once per jobs.progress_interval and only when the value actually
// changed — the same throttling reasoning as sessions.last_seen_at (10 §4.1), for the same
// single-writer reason.
func (h *Handle) Progress(ctx context.Context, pct int, message string) {
	h.mu.Lock()
	h.progress, h.message = pct, message
	changed := pct != h.lastWriteP || message != h.lastWriteM
	write := changed && time.Since(h.lastWriteAt) >= h.engine.cfg.ProgressInterval
	if write {
		h.lastWriteP, h.lastWriteM, h.lastWriteAt = pct, message, time.Now()
	}
	h.mu.Unlock()

	h.engine.broker.publish(h.jobID, Event{JobID: h.jobID, Status: "running", Progress: pct, Message: message})
	if !write {
		return
	}
	if err := h.engine.db.UpdateJobProgress(ctx, h.jobID, pct, message); err != nil {
		slog.WarnContext(ctx, "write job progress", slog.String("job_id", h.jobID), slog.Any("error", err))
	}
}

// Log appends one line to the in-memory tail and fans it out live. It never touches the
// database — 12 §7's log column is written exactly once, at Finish.
func (h *Handle) Log(line string) {
	h.mu.Lock()
	h.log.Append(line)
	h.mu.Unlock()
	h.engine.broker.publish(h.jobID, Event{JobID: h.jobID, LogLine: line})
}

// Checkpoint records a resume marker (12 §9.4). Unlike Progress it is never throttled: a
// kind that uses this crosses only a handful of named checkpoints in its whole run, so
// there is no firehose to protect the writer pool from.
func (h *Handle) Checkpoint(ctx context.Context, checkpoint string) error {
	if err := h.engine.db.UpdateJobCheckpoint(ctx, h.jobID, checkpoint); err != nil {
		return fmt.Errorf("checkpoint job %s at %s: %w", h.jobID, checkpoint, err)
	}
	return nil
}

// CancelRequested reports whether the job's cancel_requested_at is set. A Runner checks
// this at its kind's declared points of no return (12 §8) — the engine cannot know where
// those are, so it never checks on the Runner's behalf.
func (h *Handle) CancelRequested(ctx context.Context) bool {
	j, err := h.engine.db.JobByID(ctx, h.jobID)
	if err != nil || j == nil {
		return false
	}
	return j.CancelRequestedAt != nil
}

func (h *Handle) snapshot() (progress int, message, log string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.progress, h.message, h.log.String()
}
