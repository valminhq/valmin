package instance

import (
	"time"

	"github.com/valminhq/valmin/internal/jobs"
)

// Reality is what Docker says about one instance's container — the input side of both
// 08 §6.1's reconciliation and 12 §9.2's recovery matrix. Docker, not the panel, is the
// source of truth for what is running (08 §6.1); this is that truth, reduced to the four
// facts the two tables actually branch on.
type Reality struct {
	// Found is false when the instance names no container, or names one Docker no longer
	// has. Both are "container gone" to every row that reads it.
	Found        bool
	Running      bool
	OOMKilled    bool
	RestartCount int
	// CrashLooping is 08 §6's second guard. It is not a field Docker exposes: RestartCount
	// is cumulative, so "above a threshold within a window" needs the panel's own memory of
	// what it last saw. CrashLoop below computes it.
	CrashLooping bool
}

// Verdict is what the caller must do about one instance. The zero Verdict means "leave the
// row alone", which is the answer for every instance whose state already matches reality.
type Verdict struct {
	// To is the state to move to, or "" to leave the row where it is.
	To State
	// Reason is the human-readable why: logged, and carried into the instance's warning.
	Reason string
	// Stop asks the caller to stop the container before writing To. It is 08 §6's guard
	// against `unless-stopped`: Docker will happily resurrect a container the panel wants
	// parked in `error`, so parking it is only half the job.
	Stop bool
	// Recheck asks the caller to re-establish readiness before committing To — 12 §9.2's
	// `starting` + running row, which is a question about the log, not about Docker's own
	// state, and so cannot be answered here.
	Recheck bool
	// Rerun names a job kind the caller must re-submit, with To as the fallback when it
	// cannot: 12 §9.2's `provisioning` ("resume from checkpoint if one exists, else
	// error") and `deleting` ("re-run the delete; it is idempotent").
	Rerun jobs.Kind
}

// Observe answers 12 §2.2's four observation rows and 12 §9.2's recovery matrix in one
// switch, because they are one question asked at two moments: *what does this row have to
// become for it to agree with Docker again?*
//
// `↯` The caller decides which instances to ask about, and that is where C14 lives: while a
// lock is held, the observer does not write. During a job the container will exit because
// the job stopped it, and an observer that independently flips `running → stopped` on that
// event races the job that caused it. Every transient row below is therefore only ever
// reached for an instance whose lock is free — at startup, after the dead-job sweep
// (12 §9.1 step 2), or in steady state for a job whose worker died without finishing.
func Observe(state State, r Reality) Verdict {
	switch state {
	// `created` has no container to disagree with, and `error` is a parking state whose only
	// exit is a human (12 §2.4) — the observer must never move either.
	case StateCreated, StateError:
		return Verdict{}

	// Durable states: 12 §2.2's observations, 08 §6.1 step 3's first four bullets.
	case StateRunning:
		return observeRunning(r)
	case StateStopped:
		if r.up() {
			return Verdict{To: StateRunning, Reason: "container is running; Docker wins"}
		}
		return Verdict{}

	// Transient states: 12 §9.2's recovery matrix, every row. The ones whose job kinds arrive
	// at M4 are written now because the branch that is never written is the branch that is
	// wrong at M4.
	case StateProvisioning:
		return Verdict{
			Rerun:  jobs.KindProvision,
			To:     StateError,
			Reason: "provisioning was interrupted and cannot be resumed",
		}
	case StateStarting:
		if r.up() {
			return Verdict{To: StateRunning, Recheck: true, Reason: "start was interrupted; rechecking readiness"}
		}
		return Verdict{To: StateStopped, Reason: "the start did not happen"}
	case StateStopping:
		if r.up() {
			return Verdict{To: StateError, Reason: "a stop was requested and demonstrably did not complete"}
		}
		return Verdict{To: StateStopped, Reason: "container exited"}
	case StateBackingUp:
		// `↯` 12 §9.2 gives a hot copy (§2.3) this row, even though §2.3 is equally clear
		// that a hot copy never enters `backing_up` in the first place. Kept as the pack
		// writes it: a recovery matrix that only handles the states the current design can
		// produce is the one that is wrong after the design changes.
		if r.up() {
			return Verdict{To: StateRunning, Reason: "hot copy was interrupted; the server never stopped"}
		}
		return Verdict{To: StateStopped, Reason: "backup was interrupted; the world was never touched"}
	case StateRestoring:
		// `↯` `error` regardless of what Docker says (B7). On-disk state is unproven, and
		// auto-starting a server whose world may be half-swapped writes new data on top of
		// a corrupt save — which turns a recoverable situation into an unrecoverable one.
		return Verdict{To: StateError, Reason: "a restore was interrupted; the world on disk is unproven"}
	case StateUpdating:
		// Never auto-start either (12 §9.2), but `server/` is disposable by design
		// (08 §4.1), so the resting state is `stopped` rather than `error`.
		return Verdict{To: StateStopped, Reason: "a game update was interrupted"}
	case StateDeleting:
		// No To: `deleting` has no successor but the row not existing at all (12 §2.1), so
		// a delete that cannot be re-run stays where it is for the next attempt.
		return Verdict{Rerun: jobs.KindDelete, Reason: "delete was interrupted"}
	}
	return Verdict{}
}

// up is "there is a container and it is running", the single question most of the matrix
// branches on. A container Docker no longer has is not running, and saying so once here
// keeps every row from having to remember it.
func (r Reality) up() bool { return r.Found && r.Running }

// observeRunning is 08 §6.1 step 3's second bullet plus 08 §6's two guards.
//
// `↯` A non-zero exit that is neither an OOM-kill nor a crash loop still lands in
// `stopped`. 12 §2.2's guard cell reads "clean exit code, no OOM, no crash loop", which
// would send every crash to `error`; 08 §6.1 is the narrower and later statement —
// "`stopped`, or `error` if OOM/crash-loop" — and it is the one that composes with
// `unless-stopped`, which will restart a crashed server before the panel can park it. The
// crash-loop guard is what catches a server that keeps doing it. Contradiction written back
// to 12 §2.2 (30 Aug 2026, WP-M1-15).
func observeRunning(r Reality) Verdict {
	if !r.Found {
		return Verdict{To: StateError, Reason: "container disappeared"}
	}
	if r.OOMKilled {
		return Verdict{
			To:     StateError,
			Stop:   true,
			Reason: "container was killed by the out-of-memory killer; the world may be damaged",
		}
	}
	if r.CrashLooping {
		return Verdict{
			To:     StateError,
			Stop:   true,
			Reason: "container is restarting repeatedly",
		}
	}
	if !r.Running {
		return Verdict{To: StateStopped, Reason: "container exited"}
	}
	return Verdict{}
}

// CrashLoopThreshold and CrashLoopWindow are 08 §6's "RestartCount above a threshold within
// a window", which the pack leaves as words. Three automatic restarts inside ten minutes is
// a server that cannot stay up; `unless-stopped` will keep trying forever, and each attempt
// is another chance to write over a damaged world (03 §3.3). Constants rather than config
// keys: no operator has asked to tune them, and 10 §1.1 gains a key the day one does.
const (
	CrashLoopThreshold = 3
	CrashLoopWindow    = 10 * time.Minute
)

// CrashLoop turns Docker's cumulative RestartCount into 08 §6's windowed guard.
//
// `↯` The window needs the panel's own memory. Docker resets RestartCount only on a manual
// start, so the count alone cannot tell three restarts in the last minute from three spread
// over six months — and parking a healthy server in `error` because it was restarted twice
// last spring is worse than missing a loop.
//
// Not safe for concurrent use: it is owned by the single observer goroutine.
type CrashLoop struct {
	Threshold int
	Window    time.Duration
	marks     map[string]crashMark
}

type crashMark struct {
	count int
	at    time.Time
}

// NewCrashLoop builds the guard at 08 §6's defaults.
func NewCrashLoop() *CrashLoop {
	return &CrashLoop{Threshold: CrashLoopThreshold, Window: CrashLoopWindow, marks: map[string]crashMark{}}
}

// Looping records containerID's current restart count and reports whether it has risen by
// Threshold within Window. A count that falls — Docker resets it on a manual start, so
// every panel-initiated start clears the slate — rebases rather than reporting a loop.
func (c *CrashLoop) Looping(containerID string, restartCount int, now time.Time) bool {
	mark, seen := c.marks[containerID]
	if !seen || restartCount < mark.count || now.Sub(mark.at) > c.Window {
		c.marks[containerID] = crashMark{count: restartCount, at: now}
		return false
	}
	return restartCount-mark.count >= c.Threshold
}

// Forget drops containerID's mark, so a container the panel has deliberately stopped and
// removed does not leave a baseline behind for whatever reuses its id.
func (c *CrashLoop) Forget(containerID string) { delete(c.marks, containerID) }
