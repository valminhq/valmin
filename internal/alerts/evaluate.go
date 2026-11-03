package alerts

import (
	"strconv"
	"strings"
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
	// BackupSchedules is one entry per enabled schedule that takes archives, cron already
	// reduced to an interval by the caller that owns the expression.
	BackupSchedules []BackupSchedule
	// LastBackups is each instance's newest consistent archive.
	LastBackups map[string]time.Time
	// FlaggedBackups is each instance's newest archive, other than a snapshot, when the world
	// check flagged it.
	FlaggedBackups map[string]store.Backup
	// Incidents is each instance's observed stops on its own, newest first.
	Incidents map[string][]store.Incident
	// FailedMods is, per instance, the mods its latest boot could not load.
	FailedMods map[string]FailedMods
	// ModUpdates is, per instance, the mod updates the catalogue offers, one line each.
	ModUpdates map[string][]string
	// InstalledBuilds is the build each instance actually runs, by instance id.
	InstalledBuilds map[string]string
	// BranchBuilds is the last observed build of each listed Steam branch, by branch name.
	BranchBuilds map[string]string
	FreeBytes    uint64
	// AlarmBytes is the floor below which free space is low. Zero disables the check.
	AlarmBytes uint64
	Now        time.Time
}

// FailedMods is what one boot's mod loader refused: the mods it named, and the loader's line for
// the first mod that failed on its own.
type FailedMods struct {
	Names  []string
	Reason string
}

// BackupSchedule is one instance's backup cadence.
type BackupSchedule struct {
	InstanceID string
	Interval   time.Duration
	// Missed is how many times the schedule has fired, on its own clock, since the instance's
	// last backup. It is what staleness is judged on, since a schedule's gaps need not be even.
	Missed int
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
	out = append(out, flaggedBackups(s)...)
	out = append(out, modsFailed(s)...)
	out = append(out, modUpdates(s)...)
	out = append(out, stuckJobs(s, params)...)
	return out
}

func lowDisk(s *Snapshot) []Condition {
	if s.AlarmBytes == 0 || s.FreeBytes >= s.AlarmBytes {
		return nil
	}
	return []Condition{{Kind: KindLowDisk, Detail: map[string]string{
		DetailFree:  strconv.FormatUint(s.FreeBytes, 10),
		DetailAlarm: strconv.FormatUint(s.AlarmBytes, 10),
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
			c := Condition{Kind: KindInstanceError, InstanceID: inst.ID}
			if cause := errorCause(s, inst.ID); cause != "" {
				c.Detail = map[string]string{DetailReason: cause, DetailMemLimit: strconv.Itoa(inst.MemLimitMB)}
			}
			out = append(out, c)
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
	installed, available := s.InstalledBuilds[inst.ID], s.BranchBuilds[inst.GameBranch]
	if !knownBuild(installed) || !knownBuild(available) || installed == available {
		return Condition{}, false
	}
	return Condition{Kind: KindUpdateAvailable, InstanceID: inst.ID, Detail: map[string]string{
		DetailInstalled: installed,
		DetailAvailable: available,
	}}, true
}

func crashLoop(s *Snapshot, instanceID string, p Params, now time.Time) (Condition, bool) {
	cutoff := now.Add(-p.CrashWindow)
	n := 0
	for _, incident := range s.Incidents[instanceID] {
		if incident.At.After(cutoff) {
			n++
		}
	}
	if n < p.CrashCount {
		return Condition{}, false
	}
	return Condition{Kind: KindCrashLoop, InstanceID: instanceID, Detail: map[string]string{
		DetailStops:  strconv.Itoa(n),
		DetailWindow: p.CrashWindow.String(),
	}}, true
}

func failedJobs(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.LatestTerminalJobs))
	for i := range s.LatestTerminalJobs {
		j := &s.LatestTerminalJobs[i]
		// A failed delivery is reported under its webhook's recent deliveries; raising an
		// alert for it would post about a channel that cannot be posted to.
		if j.Status != jobs.StatusFailed || j.Kind == jobs.KindWebhookDeliver.String() {
			continue
		}
		detail := map[string]string{DetailJob: j.Kind}
		if j.ErrorCode != nil {
			detail["Error"] = *j.ErrorCode
		}
		if j.Error != nil {
			detail[DetailReason] = *j.Error
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
			Detail:     map[string]string{DetailJob: j.Kind},
		})
	}
	return out
}

// staleBackups flags a schedule that is not producing archives. An instance with no enabled
// schedule that takes archives is never flagged: backing up by hand is a choice, not an
// incident.
func staleBackups(s *Snapshot, params func(Kind, string) Params) []Condition {
	out := make([]Condition, 0, len(s.BackupSchedules))
	for _, sc := range s.BackupSchedules {
		// A schedule the clock has never reached cannot be behind, whatever its interval.
		if sc.LastRunAt == nil || sc.Interval <= 0 {
			continue
		}
		p := params(KindStaleBackup, sc.InstanceID)
		last, ok := s.LastBackups[sc.InstanceID]
		if ok && float64(sc.Missed) < p.StaleFactor {
			continue
		}
		detail := map[string]string{DetailEvery: sc.Interval.String()}
		if ok {
			detail[DetailLast] = last.UTC().Format(time.RFC3339)
		}
		out = append(out, Condition{Kind: KindStaleBackup, InstanceID: sc.InstanceID, Detail: detail})
	}
	return out
}

// flaggedBackups reports each server whose newest archive needs a look, with what was found.
func flaggedBackups(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.FlaggedBackups))
	for instanceID := range s.FlaggedBackups {
		b := s.FlaggedBackups[instanceID]
		messages := make([]string, 0, len(b.WorldAttention))
		for _, a := range b.WorldAttention {
			messages = append(messages, a.Message)
		}
		out = append(out, Condition{Kind: KindBackupAttention, InstanceID: instanceID, Detail: map[string]string{
			DetailReason: strings.Join(messages, "; "),
			DetailBackup: b.CreatedAt.UTC().Format(time.RFC3339),
		}})
	}
	return out
}

// errorCause is the reason the observer gave for an instance's newest incident, when no job
// finished after it. An error a job left behind is the job's to explain, under job_failed.
func errorCause(s *Snapshot, instanceID string) string {
	incidents := s.Incidents[instanceID]
	if len(incidents) == 0 {
		return ""
	}
	newest := incidents[0]
	for i := range s.LatestTerminalJobs {
		j := &s.LatestTerminalJobs[i]
		if deref(j.InstanceID) == instanceID && j.FinishedAt != nil && j.FinishedAt.After(newest.At) {
			return ""
		}
	}
	return newest.Reason
}

// modsFailed reports each server whose latest boot left mods unloaded, naming them.
func modsFailed(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.FailedMods))
	for instanceID, f := range s.FailedMods {
		if len(f.Names) == 0 {
			continue
		}
		out = append(out, Condition{Kind: KindModsFailed, InstanceID: instanceID, Detail: map[string]string{
			DetailMods:   strings.Join(f.Names, ", "),
			DetailReason: f.Reason,
		}})
	}
	return out
}

// modUpdates reports each server the catalogue has newer mod versions for.
func modUpdates(s *Snapshot) []Condition {
	out := make([]Condition, 0, len(s.ModUpdates))
	for instanceID, updates := range s.ModUpdates {
		if len(updates) == 0 {
			continue
		}
		out = append(out, Condition{Kind: KindModUpdates, InstanceID: instanceID, Detail: map[string]string{
			DetailMods:  strings.Join(updates, ", "),
			DetailCount: strconv.Itoa(len(updates)),
		}})
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
			DetailJob:     j.Kind,
			DetailRunning: running.Round(time.Minute).String(),
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
