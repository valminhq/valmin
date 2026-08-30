package instance

import (
	"sort"

	"github.com/valminhq/valmin/internal/jobs"
)

// State is one of 12 §2.1's eleven states. A plain string, checked against the DB's own
// CHECK constraint and against Edges below — not a closed-registry struct like authz.Action
// or jobs.Kind, because nothing here needs "an unknown value is a compile error"; the
// transition table is the one source of truth callers validate against.
type State string

const (
	StateCreated      State = "created"
	StateProvisioning State = "provisioning"
	StateStopped      State = "stopped"
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateStopping     State = "stopping"
	StateBackingUp    State = "backing_up"
	StateRestoring    State = "restoring"
	StateUpdating     State = "updating"
	StateDeleting     State = "deleting"
	StateError        State = "error"
)

// edge is one row of 12 §2.2's transition table.
type edge struct{ from, to State }

// Edges is 12 §2.2, transcribed in full — every row, job-driven and observed alike, as one
// (from, to) adjacency. Deliberately flat rather than keyed by trigger: by the time
// something is about to write instances.state, it already knows which trigger applies: the
// question this answers is only "is that write ever legal at all."
//
// `↯` Three edges are not in 12 §2.2's table. All three are required elsewhere in the pack,
// and are added here rather than left as gaps a table-driven test cannot see.
//
// stopping -> starting. 12 §3.1 requires it — `restart` is scoped to `running`, entered
// `stopping→starting` — but §2.2's own table never lists a restart row, only
// provision/start/stop/backup/restore/game_update/delete/acknowledge (found 30 Aug 2026,
// WP-M1-11).
//
// starting -> stopped. 12 §9.2's recovery matrix requires it verbatim: `starting` with the
// container not running resolves to `stopped`, "the start simply did not happen". §2.2 gives
// `starting` only `running` and `error`, so a crash between claiming a start and the
// container actually starting had no legal resolution at all (found 30 Aug 2026, WP-M1-15).
//
// backing_up -> running. 12 §9.2's matrix has the row (an interrupted hot copy, whose server
// never stopped) while §2.3 says a hot copy never enters `backing_up` at all. Both are kept:
// the matrix is what runs when the design and reality have already diverged (found
// 30 Aug 2026, WP-M1-15).
var edgeList = []edge{
	{StateCreated, StateProvisioning}, // provision claims
	{StateProvisioning, StateStopped}, // provision succeeds
	{StateProvisioning, StateError},   // provision fails or is cancelled
	{StateStopped, StateStarting},     // start claims
	{StateStarting, StateRunning},     // ready (12 §3.3)
	{StateStarting, StateError},       // readiness deadline, or the container exits
	{StateStarting, StateStopped},     // interrupted start, container never ran — see above
	{StateRunning, StateStopping},     // stop claims (also restart claims)
	{StateStopping, StateStarting},    // restart's internal continuation — see above
	{StateStopping, StateStopped},     // container exited
	{StateStopping, StateError},       // stop timeout exceeded
	{StateRunning, StateStopped},      // container exited on its own
	{StateRunning, StateError},        // OOM-kill, crash loop, or the container disappeared
	{StateStopped, StateRunning},      // container observed running; Docker wins
	{StateStopped, StateBackingUp},    // backup claims
	{StateStopped, StateRestoring},    // restore claims
	{StateStopped, StateUpdating},     // game_update claims
	{StateBackingUp, StateStopped},    // job succeeds or fails — the world was untouched
	{StateBackingUp, StateRunning},    // interrupted hot copy — see below
	{StateRestoring, StateStopped},    // job succeeds
	{StateRestoring, StateError},      // job fails — on-disk state is unproven
	{StateUpdating, StateStopped},     // job succeeds
	{StateUpdating, StateError},       // job fails
	{StateStopped, StateDeleting},     // delete claims
	{StateError, StateDeleting},       // delete claims
	{StateError, StateStopped},        // acknowledge, reconciled to stopped
	{StateError, StateRunning},        // acknowledge, reconciled to running
}

var edges = func() map[edge]bool {
	m := make(map[edge]bool, len(edgeList))
	for _, e := range edgeList {
		m[e] = true
	}
	return m
}()

// Valid reports whether the panel may ever write instances.state from from to to. deleting
// has no outgoing edges — its only successor is the row not existing at all.
func Valid(from, to State) bool { return edges[edge{from, to}] }

// requires is 12 §3.1's "Requires" column, transcribed directly rather than derived from
// Edges.
//
// `↯` It cannot be derived: `start` may only be claimed from `stopped`, but `starting` is
// also reachable from `stopping` — restart's own internal continuation, not a client
// claiming `start`. Two different triggers land on the same state, with different valid
// callers, so a reverse lookup over Edges would (and during WP-M1-11's own tests, did)
// hand `start` an extra, wrong entry. Kept as its own small table instead of clever.
var requires = map[jobs.Kind][]State{
	jobs.KindProvision: {StateCreated},
	jobs.KindStart:     {StateStopped},
	jobs.KindStop:      {StateRunning},
	jobs.KindRestart:   {StateRunning},
	jobs.KindDelete:    {StateStopped, StateError},
}

// AllowedFrom returns, sorted, the states kind may be claimed from — the `allowed_states`
// of a 409 invalid_state (11 §2.5, 12 §3.1).
func AllowedFrom(kind jobs.Kind) []State {
	from := append([]State{}, requires[kind]...)
	sort.Slice(from, func(i, j int) bool { return from[i] < from[j] })
	return from
}
