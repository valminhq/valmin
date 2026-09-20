package alerts

import (
	"strconv"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Snapshot is everything Evaluate may look at. The caller gathers it, including the one statfs,
// before opening any transaction (C1).
type Snapshot struct {
	Instances []store.Instance
	// LatestTerminalJobs is the newest terminal job per instance and kind, so a superseded
	// failure is already absent.
	LatestTerminalJobs []store.Job
	// CleanSignals is the newest job per instance that recorded a clean flag. A backup between
	// two stops carries none and must not shadow the stop that does.
	CleanSignals []store.Job
	RunningJobs  []store.Job
	// BackupSchedules is one entry per enabled backup schedule, cron already reduced to an
	// interval by the caller that owns the expression.
	BackupSchedules []BackupSchedule
	LastBackups     map[string]time.Time
	// Incidents is when each instance was observed going down on its own, newest first.
	Incidents map[string][]time.Time
	// InstalledBuilds is the build each instance actually runs, by instance id.
	InstalledBuilds map[string]string
	PublicBuild     string
	FreeBytes       uint64
	// AlarmBytes is the floor below which free space is low. Zero disables the check.
	AlarmBytes uint64
	Now        time.Time
}

// BackupSchedule is one instance's backup cadence.
type BackupSchedule struct {
	InstanceID string
	Interval   time.Duration
	// LastRunAt is when the clock last considered this schedule, nil if it never has.
	LastRunAt *time.Time
}

// Evaluate reports every condition currently true. resolve supplies each instance's thresholds;
// a nil resolver means the defaults throughout.
func Evaluate(s *Snapshot, resolve Resolver) []Condition {
	if resolve == nil {
		resolve = func(Kind, string) Params { return Params{} }
	}
	params := func(k Kind, id string) Params { return resolve(k, id).Defaults() }

	out := make([]Condition, 0, len(s.Instances))
	out = append(out, lowDisk(s)...)
	out = append(out, perInstance(s, params)...)
	out = append(out, failedJobs(s)...)
	out = append(out, uncleanStops(s)...)
	out = append(out, staleBackups(s, params)...)
	out = append(out, stuckJobs(s, params)...)
	return out
}

func lowDisk(s *Snapshot) []Condition {
	if s.AlarmBytes == 0 || s.FreeBytes >= s.AlarmBytes {
		return nil
	}
	return []Condition{{Kind: KindLowDisk, Detail: map[string]string{
		"Free":  strconv.FormatUint(s.FreeBytes, 10),
		"Alarm": strconv.FormatUint(s.AlarmBytes, 10),
	}}}
}

// perInstance covers the conditions readable off the instance row plus the crash rate, walking
// the list once.
func perInstance(s *Snapshot, params func(Kind, string) Params) []Condition {
	out := make([]Condition, 0, len(s.Instances))
	for i := range s.Instances {
		inst := &s.Instances[i]
		if inst.RestartRequired {
			out = append(out, Condition{Kind: KindRestartRequired, InstanceID: inst.ID})
		}
		if inst.State == stateError {
			out = append(out, Condition{Kind: KindInstanceError, InstanceID: inst.ID})
		}
		if c, ok := updateAvailable(s, inst); ok {
			out = append(out, c)
		}
		if c, ok := crashLoop(s, inst.ID, params(KindCrashLoop, inst.ID), s.Now); ok {
			out = append(out, c)
		}
	}
	return out
}

func updateAvailable(s *Snapshot, inst *store.Instance) (Condition, bool) {
	installed := s.InstalledBuilds[inst.ID]
	if !knownBuild(installed) || !knownBuild(s.PublicBuild) || installed == s.PublicBuild {
		return Condition{}, false
	}
	return Condition{Kind: KindUpdateAvailable, InstanceID: inst.ID, Detail: map[string]string{
		"Installed": installed,
		"Available": s.PublicBuild,
	}}, true
}

func crashLoop(s *Snapshot, instanceID string, p Params, now time.Time) (Condition, bool) {
	cutoff := now.Add(-p.CrashWindow)
	n := 0
	for _, at := range s.Incidents[instanceID] {
		if at.After(cutoff) {
			n++
		}
	}
	if n < p.CrashCount {
		return Condition{}, false
	}
	return Condition{Kind: KindCrashLoop, InstanceID: instanceID, Detail: map[string]string{
		"Stops":  strconv.Itoa(n),
		"Window": p.CrashWindow.String(),
	}}, true
}

func failedJobs(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.LatestTerminalJobs))
	for i := range s.LatestTerminalJobs {
		j := &s.LatestTerminalJobs[i]
		if j.Status != jobs.StatusFailed {
			continue
		}
		detail := map[string]string{"Job": j.Kind}
		if j.ErrorCode != nil {
			detail["Error"] = *j.ErrorCode
		}
		out = append(out, Condition{Kind: KindJobFailed, InstanceID: deref(j.InstanceID), Detail: detail})
	}
	return out
}

func uncleanStops(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.CleanSignals))
	for i := range s.CleanSignals {
		j := &s.CleanSignals[i]
		if j.Clean == nil || *j.Clean {
			continue
		}
		out = append(out, Condition{
			Kind:       KindUncleanStop,
			InstanceID: deref(j.InstanceID),
			Detail:     map[string]string{"Job": j.Kind},
		})
	}
	return out
}

// staleBackups flags a schedule that is not producing archives. An instance with no enabled
// backup schedule is never flagged: backing up by hand is a choice, not an incident.
func staleBackups(s *Snapshot, params func(Kind, string) Params) []Condition {
	out := make([]Condition, 0, len(s.BackupSchedules))
	for _, sc := range s.BackupSchedules {
		// A schedule the clock has never reached cannot be behind, whatever its interval.
		if sc.LastRunAt == nil || sc.Interval <= 0 {
			continue
		}
		p := params(KindStaleBackup, sc.InstanceID)
		age := time.Duration(float64(sc.Interval) * p.StaleFactor)
		last, ok := s.LastBackups[sc.InstanceID]
		if ok && s.Now.Sub(last) <= age {
			continue
		}
		detail := map[string]string{"Every": sc.Interval.String()}
		if ok {
			detail["Last"] = last.UTC().Format(time.RFC3339)
		}
		out = append(out, Condition{Kind: KindStaleBackup, InstanceID: sc.InstanceID, Detail: detail})
	}
	return out
}

func stuckJobs(s *Snapshot, params func(Kind, string) Params) []Condition {
	out := make([]Condition, 0, len(s.RunningJobs))
	for i := range s.RunningJobs {
		j := &s.RunningJobs[i]
		if j.StartedAt == nil {
			continue
		}
		instanceID := deref(j.InstanceID)
		running := s.Now.Sub(*j.StartedAt)
		if running <= params(KindJobStuck, instanceID).StuckAfter {
			continue
		}
		out = append(out, Condition{Kind: KindJobStuck, InstanceID: instanceID, Detail: map[string]string{
			"Job":     j.Kind,
			"Running": running.Round(time.Minute).String(),
		}})
	}
	return out
}

// stateError is instance.StateError's wire value. Spelled out rather than imported so that the
// evaluator depends on no package that talks to Docker.
const stateError = "error"

// knownBuild reports whether a build id is a real Steam build number. Legacy rows carry a cache
// alias rather than a version, and comparing one of those against a build id would report an
// update that does not exist.
func knownBuild(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
