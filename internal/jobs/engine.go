// Package jobs owns the job engine, locks, leases and crash recovery.
//
// Specification: 12.
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
	ctx    context.Context
	cancel context.CancelFunc

	workersMu sync.Mutex
	workers   sync.WaitGroup
	draining  bool

	mu       sync.Mutex
	policies map[Kind]CancelPolicy
	announce func(ctx context.Context, instanceID string)
	onFinish func(ctx context.Context, tx *sql.Tx, job *FinishedJob) error
}

// Announce registers the state publisher of 14 §4.4. The engine is one of the two writers of
// instances.state (12 §1), and calls this only after a claim or finish transaction has
// committed, never from inside one.
//
// Two call sites here cover every job-driven flip, rather than one at each OnClaim and
// OnFinish.
func (e *Engine) Announce(fn func(ctx context.Context, instanceID string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.announce = fn
}

// announced publishes a state change for an instance, if a publisher is registered.
func (e *Engine) announced(ctx context.Context, instanceID *string) {
	if instanceID == nil || *instanceID == "" {
		return
	}
	e.mu.Lock()
	fn := e.announce
	e.mu.Unlock()
	if fn != nil {
		fn(ctx, *instanceID)
	}
}

// New builds an Engine. owner is "<panel_id>:<boot_id>" (store.Owner) — the same value
// passed to the daemon lease, so both crash markers agree on which process is asking.
func New(db *store.DB, owner string, cfg Config) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		db: db, owner: owner, cfg: cfg, broker: newBroker(), ctx: ctx, cancel: cancel,
		policies: map[Kind]CancelPolicy{},
	}
}

// Spec describes one job submission — 12 §6's Claim phase.
type Spec struct {
	Kind    Kind
	LockKey string
	// LockKeys are supplemental locks acquired atomically with LockKey. LockKey remains the
	// canonical key stored on job_runs and exposed by the API.
	LockKeys     []string
	InstanceID   *string
	InstanceName string
	Payload      any
	// RequestedBy is a user id, or "" for the scheduler, which writes NULL: a job nobody
	// asked for must say so rather than borrow the last operator who touched the panel.
	RequestedBy string
	// ScheduleID is the scheduled_jobs row whose tick enqueued this, "" for a job a person
	// asked for (12 §11).
	ScheduleID string
	// ResumeAfter records that the server was running when this job claimed it, and so owes
	// the user a restart if the panel dies mid-job. Honoured on recovery only for world-safe
	// kinds (12 §9.3, ADR-032).
	ResumeAfter bool
	// OnClaim runs inside the same transaction as the lock and job-row insert. 12 §6
	// requires a side effect like an instance's transient state to land atomically with
	// the job starting; the engine does not know that enum, so the caller supplies it.
	OnClaim func(context.Context, *sql.Tx) error
}

// The terminal job statuses written to job_runs.status (12 §3.1). Untyped so they remain
// assignable wherever the status crosses into the store and the wire.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Outcome is what a Runner returns: the terminal status and, if it failed, the registry
// code and message that explain why.
type Outcome struct {
	Status    string // one of StatusSucceeded, StatusFailed, StatusCancelled
	ErrorCode string
	Error     string
	// Clean is 12 §3.4's clean-completion signal, recorded on the job row (nil where the
	// kind has no such concept).
	Clean *bool
	// OnFinish runs inside the Finish transaction, alongside the terminal status and lock
	// release — the seam for a side-effect row (12 §6's corollary: written from data
	// already in memory, never from a read inside the transaction).
	OnFinish func(context.Context, *sql.Tx) error
	// AfterFinish runs once that transaction has committed and the lock is released, which is
	// what 12 §2.2's chained start and §9.3's resume intent need: a job cannot submit another on
	// its own lock key while holding it. Never load-bearing; a failure here is only logged.
	AfterFinish func(context.Context)
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

// ErrShuttingDown reports that the engine no longer accepts work.
var ErrShuttingDown = errors.New("job engine is shutting down")

// ErrNotCancellable reports a running job past its declared point of no return (12 §8).
type ErrNotCancellable struct{ Phase string }

func (e *ErrNotCancellable) Error() string {
	return fmt.Sprintf("not cancellable past %s", e.Phase)
}

// Submit is 12 §6's Claim phase plus dispatch. The lock and job row land in one transaction
// before this returns, so two concurrent submissions on the same LockKey collide correctly
// (ADR-030), and the Runner then executes in its own goroutine. A returned *store.Job means the
// lock is held, not that the Runner has started (11 §3).
//
// A collision comes back as *store.JobConflict, unwrapped with errors.As, carrying the active
// job's id and kind for the caller's 409.
func (e *Engine) Submit(ctx context.Context, spec *Spec, run Runner) (*store.Job, error) {
	payload, err := json.Marshal(spec.Payload)
	if err != nil {
		return nil, fmt.Errorf("encode job payload: %w", err)
	}
	var requestedBy, scheduleID *string
	if spec.RequestedBy != "" {
		requestedBy = &spec.RequestedBy
	}
	if spec.ScheduleID != "" {
		scheduleID = &spec.ScheduleID
	}

	j := &store.Job{
		ID:           store.NewID(),
		Kind:         spec.Kind.name,
		LockKey:      spec.LockKey,
		InstanceID:   spec.InstanceID,
		InstanceName: spec.InstanceName,
		ScheduleID:   scheduleID,
		Payload:      string(payload),
		ResumeAfter:  spec.ResumeAfter,
		RequestedBy:  requestedBy,
	}
	e.workersMu.Lock()
	if e.draining {
		e.workersMu.Unlock()
		return nil, ErrShuttingDown
	}

	leaseUntil := time.Now().Add(e.cfg.LeaseTTL)
	if err := e.db.ClaimJobWithLocks(ctx, j, spec.LockKeys, e.owner, leaseUntil, spec.OnClaim); err != nil {
		e.workersMu.Unlock()
		var conflict *store.JobConflict
		if errors.As(err, &conflict) {
			return nil, conflict
		}
		return nil, fmt.Errorf("claim job: %w", err)
	}
	e.workers.Add(1)
	e.workersMu.Unlock()

	// The claim transaction has committed, so the transient state OnClaim wrote is real.
	e.announced(ctx, spec.InstanceID)

	// The work outlives its HTTP request and is cancelled only by lease loss or daemon shutdown.
	go func() {
		defer e.workers.Done()
		e.run(context.WithoutCancel(ctx), j.ID, spec.InstanceID, e.hooked(j.ID, spec, run))
	}()
	return j, nil
}

// FinishedJob describes a job reaching a terminal status, as the finish hook sees it.
type FinishedJob struct {
	ID         string
	Kind       Kind
	InstanceID *string
	// InstanceName is the name the job was submitted against, so a hook can name the server
	// in a message without reading a row inside the finish transaction.
	InstanceName string
	Payload      any
	Status       string
}

// OnFinish registers a hook run inside every job's Finish transaction, after the job's own
// Outcome.OnFinish. It is the seam a chain of jobs uses to record its progress atomically
// with the step that completed (Q52); the hook holds the writer connection, so C1 applies to
// it exactly as it does to OnFinish.
func (e *Engine) OnFinish(hook func(context.Context, *sql.Tx, *FinishedJob) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onFinish = hook
}

// hooked wraps a Runner so the registered finish hook joins its Outcome.OnFinish.
func (e *Engine) hooked(jobID string, spec *Spec, run Runner) Runner {
	return func(ctx context.Context, h *Handle) Outcome {
		outcome := run(ctx, h)
		e.mu.Lock()
		hook := e.onFinish
		e.mu.Unlock()
		if hook == nil {
			return outcome
		}
		inner := outcome.OnFinish
		fin := &FinishedJob{
			ID: jobID, Kind: spec.Kind, InstanceID: spec.InstanceID,
			InstanceName: spec.InstanceName, Payload: spec.Payload, Status: outcome.Status,
		}
		outcome.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
			if inner != nil {
				if err := inner(ctx, tx); err != nil {
					return err
				}
			}
			return hook(ctx, tx, fin)
		}
		return outcome
	}
}

// run is the Work and Finish phases, entirely off the request goroutine.
func (e *Engine) run(parent context.Context, jobID string, instanceID *string, run Runner) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel) //nolint:contextcheck // The job also inherits request values from parent.
	defer stop()

	h := newHandle(e, jobID)
	leaseLost := make(chan struct{})
	go e.renewLease(ctx, jobID, cancel, leaseLost)

	outcome := run(ctx, h)

	if e.ctx.Err() != nil {
		slog.InfoContext(parent, "job interrupted by daemon shutdown",
			slog.String("job_id", jobID))
		return
	}
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
	e.announced(finishCtx, instanceID)
	if outcome.AfterFinish != nil {
		outcome.AfterFinish(finishCtx)
	}
}

// abandonGrace is how long Shutdown waits for cancelled runners to unwind.
const abandonGrace = 5 * time.Second

// Shutdown stops accepting jobs and waits for active runners. Reaching ctx's deadline cancels
// their contexts so crash recovery can resolve any unfinished rows on the next boot, then
// allows abandonGrace for them to leave the database: a runner's FinishJob is uncancellable
// (12 §6), so returning at the deadline would let the caller close it under a write in flight.
func (e *Engine) Shutdown(ctx context.Context) {
	e.workersMu.Lock()
	e.draining = true
	e.workersMu.Unlock()

	done := make(chan struct{})
	go func() {
		e.workers.Wait()
		close(done)
	}()

	select {
	case <-done:
		e.cancel()
		return
	case <-ctx.Done():
	}
	e.cancel()

	select {
	case <-done:
	case <-time.After(abandonGrace):
		slog.WarnContext(ctx, "abandoned jobs still running after cancellation")
	}
}

// Owner is "<panel_id>:<boot_id>", the value this process writes into every lease it takes.
// 12 §9.1's dead-job sweep needs it to ask which running rows belong to a process that is
// no longer here.
func (e *Engine) Owner() string { return e.owner }

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

// Subscribe follows jobID's live events for the job.{id} topic — the seam the hub uses,
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
