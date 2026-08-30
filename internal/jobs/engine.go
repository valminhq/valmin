package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// Config is what the engine needs from the operator's settings (10 §1.1's jobs.* keys).
type Config struct {
	LeaseTTL         time.Duration
	ProgressInterval time.Duration
	LogCap           int
	RetentionDays    int
}

// Engine is 12's job engine: one active job per lock key, a lease that makes a dead
// worker's claim recognisable, and the writer-pool discipline that keeps a 1 GB download
// from freezing every other write in the panel.
type Engine struct {
	db    *store.DB
	owner string
	cfg   Config

	broker *broker

	mu       sync.Mutex
	policies map[Kind]CancelPolicy
}

// New builds an Engine. owner is "<panel_id>:<boot_id>" (store.Owner) — the same value
// passed to the daemon lease, so both crash markers agree on which process is asking.
func New(db *store.DB, owner string, cfg Config) *Engine {
	return &Engine{db: db, owner: owner, cfg: cfg, broker: newBroker(), policies: map[Kind]CancelPolicy{}}
}

// Spec describes one job submission — 12 §6's Claim phase.
type Spec struct {
	Kind         Kind
	LockKey      string
	InstanceID   *string
	InstanceName string
	Payload      any
	// RequestedBy is a user id, or "" for the scheduler (NULL) — unreachable at M1, which
	// has no scheduler, but the column exists and a job created by nobody must say so.
	RequestedBy string
	// OnClaim runs inside the same transaction as the lock and job-row insert. 12 §6
	// requires a side effect like an instance's transient state to land atomically with
	// the job starting; the engine does not know that enum, so the caller supplies it.
	OnClaim func(context.Context, *sql.Tx) error
}

// Outcome is what a Runner returns: the terminal status and, if it failed, the registry
// code and message that explain why.
type Outcome struct {
	Status    string // succeeded|failed|cancelled
	ErrorCode string
	Error     string
	// Clean is 12 §3.4's clean-completion signal, recorded on the job row (nil where the
	// kind has no such concept).
	Clean *bool
	// OnFinish runs inside the Finish transaction, alongside the terminal status and lock
	// release — the seam for a side-effect row (12 §6's corollary: written from data
	// already in memory, never from a read inside the transaction).
	OnFinish func(context.Context, *sql.Tx) error
}

// Runner is the Work phase (12 §6): no transaction, ever (C1). ctx is cancelled the moment
// the job's lease is lost, which is fatal to the job (C17) — a Runner that ignores ctx
// simply runs to an outcome the engine then discards.
type Runner func(ctx context.Context, h *Handle) Outcome

// ErrJobNotFound reports that no such job exists.
var ErrJobNotFound = errors.New("job not found")

// ErrJobTerminal reports that a job has already reached a terminal status — cancelling it
// again, or a queued job that a worker won the race to claim first, is a no-op (12 §8).
var ErrJobTerminal = errors.New("job already finished")

// ErrNotCancellable reports a running job past its declared point of no return (12 §8).
type ErrNotCancellable struct{ Phase string }

func (e *ErrNotCancellable) Error() string {
	return fmt.Sprintf("not cancellable past %s", e.Phase)
}

// Submit is 12 §6's Claim phase plus dispatch. The lock and the job row land in one
// transaction before this returns — so two concurrent submissions on the same LockKey
// collide correctly (ADR-030) — and the Runner then executes in its own goroutine.
// Reaching a returned *store.Job means the lock is held; it says nothing about whether the
// Runner has started (11 §3: "a 202 means the lock is held", not that the work has begun).
//
// A collision comes back as *store.JobConflict, unwrapped with errors.As, carrying the
// active job's id and kind for the caller's 409.
func (e *Engine) Submit(ctx context.Context, spec *Spec, run Runner) (*store.Job, error) {
	payload, err := json.Marshal(spec.Payload)
	if err != nil {
		return nil, fmt.Errorf("encode job payload: %w", err)
	}
	var requestedBy *string
	if spec.RequestedBy != "" {
		requestedBy = &spec.RequestedBy
	}

	j := &store.Job{
		ID:           store.NewID(),
		Kind:         spec.Kind.name,
		LockKey:      spec.LockKey,
		InstanceID:   spec.InstanceID,
		InstanceName: spec.InstanceName,
		Payload:      string(payload),
		RequestedBy:  requestedBy,
	}
	leaseUntil := time.Now().Add(e.cfg.LeaseTTL)
	if err := e.db.ClaimJob(ctx, j, e.owner, leaseUntil, spec.OnClaim); err != nil {
		var conflict *store.JobConflict
		if errors.As(err, &conflict) {
			return nil, conflict
		}
		return nil, fmt.Errorf("claim job: %w", err)
	}

	// The work must outlive the HTTP request that triggered it (12 §6: work is minutes, a
	// request is not) — but it must still die with the daemon, so it hangs off context.
	// Background() rather than the request's, cancelled only by lease loss.
	go e.run(context.WithoutCancel(ctx), j.ID, run)
	return j, nil
}

// run is the Work and Finish phases, entirely off the request goroutine.
func (e *Engine) run(parent context.Context, jobID string, run Runner) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	h := newHandle(e, jobID)
	leaseLost := make(chan struct{})
	go e.renewLease(ctx, jobID, cancel, leaseLost)

	outcome := run(ctx, h)

	select {
	case <-leaseLost:
		// C17: losing the lease is fatal to the job, not the panel. Whoever holds the
		// lease now owns the outcome; writing a terminal status here would race them.
		slog.WarnContext(parent, "job lease lost, abandoning without a terminal status",
			slog.String("job_id", jobID))
		return
	default:
	}

	progress, _, log := h.snapshot()
	var logPtr, errCodePtr, errPtr *string
	if log != "" {
		logPtr = &log
	}
	if outcome.ErrorCode != "" {
		errCodePtr = &outcome.ErrorCode
	}
	if outcome.Error != "" {
		errPtr = &outcome.Error
	}

	finishCtx := context.WithoutCancel(parent)
	if err := e.db.FinishJob(
		finishCtx,
		jobID,
		outcome.Status,
		progress,
		errCodePtr,
		errPtr,
		logPtr,
		outcome.Clean,
		time.Now(),
		outcome.OnFinish,
	); err != nil {
		slog.ErrorContext(finishCtx, "finish job", slog.String("job_id", jobID), slog.Any("error", err))
		return
	}
	e.broker.publish(jobID, Event{JobID: jobID, Status: outcome.Status, Progress: progress})
}

// renewLease is 12 §5.2's single autocommit UPDATE, every LeaseTTL/3, outside any
// transaction. Finding zero rows affected means the lease_owner is no longer ours; it
// cancels ctx and signals leaseLost so run knows not to write a terminal status.
func (e *Engine) renewLease(ctx context.Context, jobID string, cancel context.CancelFunc, leaseLost chan<- struct{}) {
	ticker := time.NewTicker(e.cfg.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := e.db.RenewJobLease(ctx, jobID, e.owner, time.Now().Add(e.cfg.LeaseTTL))
			if err != nil {
				slog.WarnContext(ctx, "renew job lease, will retry",
					slog.String("job_id", jobID), slog.Any("error", err))
				continue
			}
			if !ok {
				close(leaseLost)
				cancel()
				return
			}
		}
	}
}

// Subscribe follows jobID's live events (job.{id}, 04 §4) — the seam WP-21's hub uses,
// ahead of that hub existing.
func (e *Engine) Subscribe(jobID string) (events <-chan Event, cancel func()) {
	return e.broker.Subscribe(jobID)
}

// Get reads one job by id, for GET /jobs/{id} (11 §3: a failed job is a 200, not an error).
func (e *Engine) Get(ctx context.Context, id string) (*store.Job, error) {
	j, err := e.db.JobByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", id, err)
	}
	return j, nil
}

// Sweep is 12 §7's retention sweep, run once at daemon start: terminal jobs older than
// jobs.retention_days or beyond the most recent 500 per instance are pruned in one DELETE.
func (e *Engine) Sweep(ctx context.Context) error {
	n, err := e.db.SweepTerminalJobs(ctx, time.Now(), e.cfg.RetentionDays)
	if err != nil {
		return fmt.Errorf("sweep terminal jobs: %w", err)
	}
	if n > 0 {
		slog.InfoContext(ctx, "pruned terminal jobs", slog.Int64("count", n))
	}
	return nil
}
