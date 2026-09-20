// Package alerts reports which operational conditions are true of the panel's instances right
// now. The operations inbox renders that answer; the alert rules dispatch on its edges.
//
// Evaluation is a pure function over a snapshot. Gathering it, persisting the diff and sending
// anything belong to the caller.
package alerts

import (
	"strconv"
	"time"
)

// Kind is a condition kind, the same closed registry jobs.Kind and authz.Action use. The wire
// name reaches webhook payloads and the inbox, so it is a published contract and never renamed.
type Kind struct{ name string }

// String is the wire form, stored in alert_conditions.kind.
func (k Kind) String() string { return k.name }

// MarshalJSON renders the kind as its wire name.
func (k Kind) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(k.name)), nil }

var (
	// KindJobFailed is a failed job no later run of its kind has superseded.
	KindJobFailed = Kind{"job_failed"}
	// KindLowDisk is free space below the alarm floor. Host-level: every data directory shares
	// one filesystem, so a per-instance reading is the same number repeated (03 §3.4).
	KindLowDisk = Kind{"low_disk"}
	// KindStaleBackup is an enabled backup schedule not producing archives. Measured against
	// the backups, not the schedule: last_run_at advances even for a tick that skipped (12 §11).
	KindStaleBackup = Kind{"stale_backup"}
	// KindUncleanStop is a stop that did not reach the save-complete line (12 §3.4, ADR-065).
	// It clears on the next clean stop, not the next start.
	KindUncleanStop = Kind{"unclean_stop"}
	// KindRestartRequired is a change waiting for the restart that applies it (ADR-012, B11).
	KindRestartRequired = Kind{"restart_required"}
	// KindUpdateAvailable is an installed game build behind the observed public branch.
	KindUpdateAvailable = Kind{"update_available"}
	// KindInstanceError is an instance parked in `error`. It outlives the job that caused it,
	// which retention eventually prunes.
	KindInstanceError = Kind{"instance_error"}
	// KindCrashLoop is repeated unexpected stops in a window. Counted from recorded incidents
	// because a server that crashes and restarts between scans is never observed down.
	KindCrashLoop = Kind{"crash_loop"}
	// KindJobStuck is a job running longer than its threshold. It reports only; the lease is
	// the engine's business (12 §6).
	KindJobStuck = Kind{"job_stuck"}
)

var all = []Kind{
	KindJobFailed, KindLowDisk, KindStaleBackup, KindUncleanStop, KindRestartRequired,
	KindUpdateAvailable, KindInstanceError, KindCrashLoop, KindJobStuck,
}

// ParseKind resolves a kind name to its constant. An unresolved name is the caller's cue to
// answer 422.
func ParseKind(name string) (Kind, bool) {
	for _, k := range all {
		if k.name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// Kinds returns every condition kind, for the rule editor's own list.
func Kinds() []Kind { return append([]Kind(nil), all...) }

// Condition is one thing currently believed true. InstanceID is empty for a host-level
// condition.
type Condition struct {
	Kind       Kind
	InstanceID string
	// Detail is the kind's own fields, rendered into notifications and the inbox row. Nothing
	// branches on it.
	Detail map[string]string
}

// Params are a kind's thresholds. Zero fields read as the defaults, so a rule written before a
// threshold existed keeps working. The wire form belongs to the API, not to this struct.
type Params struct {
	// CrashCount is how many unexpected stops inside CrashWindow make a crash loop.
	CrashCount  int
	CrashWindow time.Duration
	// StuckAfter is how long a job may run before the panel reports it.
	StuckAfter time.Duration
	// StaleFactor multiplies a backup schedule's interval to get the age at which its archives
	// are stale. Above 1, so one missed run is not an incident.
	StaleFactor float64
}

// Defaults fills every unset field, chosen to stay quiet on a healthy panel.
func (p Params) Defaults() Params {
	if p.CrashCount <= 0 {
		p.CrashCount = 3
	}
	if p.CrashWindow <= 0 {
		p.CrashWindow = 30 * time.Minute
	}
	if p.StuckAfter <= 0 {
		p.StuckAfter = time.Hour
	}
	if p.StaleFactor <= 1 {
		p.StaleFactor = 2
	}
	return p
}

// Resolver answers which thresholds apply to one instance's copy of one kind, so a noisy server
// can be tuned alone. Returning the zero Params means the defaults.
type Resolver func(kind Kind, instanceID string) Params
