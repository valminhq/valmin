// Package scan runs the alert scan job: it gathers a snapshot, evaluates every condition,
// reconciles the stored set and dispatches the edges a rule announces.
package scan

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// Dispatcher sends the notifications owed for stored alert edges.
type Dispatcher interface {
	DispatchAlerts(ctx context.Context)
}

// Scanner gathers alert state, reconciles conditions, and dispatches transitions. A nil
// Dispatcher reconciles without notifying, and a nil ModUpdates never reports mod updates.
type Scanner struct {
	DB         *store.DB
	Engine     *jobs.Engine
	DataRoot   string
	AlarmFloor func([]store.Instance) uint64
	Dispatcher Dispatcher
	// Patterns reads each modded server's loader log.
	Patterns instance.PatternSet
	// ModUpdates lists the mod updates the catalogue offers one server.
	ModUpdates func(ctx context.Context, instanceID string) ([]manager.UpdateTarget, error)
}

// Submit enqueues a global condition scan.
func (s *Scanner) Submit(ctx context.Context, scheduleID string) (*store.Job, error) {
	j, err := s.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindAlertScan, LockKey: jobs.GlobalLockKey(jobs.KindAlertScan),
		Payload: struct{}{}, ScheduleID: scheduleID,
	}, s.run)
	if err != nil {
		return nil, fmt.Errorf("submit alert scan: %w", err)
	}
	return j, nil
}

// run evaluates every condition and reconciles the stored set against it.
func (s *Scanner) run(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	jh.Progress(ctx, 10, "Reading the panel's state")
	diff, err := s.Scan(ctx)
	if err != nil {
		return jobs.Outcome{Status: jobs.StatusFailed, Error: err.Error()}
	}
	jh.Progress(ctx, 100, fmt.Sprintf("%d opened, %d resolved", len(diff.Opened), len(diff.Resolved)))
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// Scan is one scan: gather, evaluate, reconcile, dispatch what is owed. Reading is all
// done before the reconcile transaction opens, because the gather touches the filesystem.
func (s *Scanner) Scan(ctx context.Context) (store.ConditionDiff, error) {
	snapshot, resolver, err := s.snapshot(ctx)
	if err != nil {
		return store.ConditionDiff{}, err
	}

	observed := make([]store.ObservedCondition, 0, 8)
	for _, c := range alerts.Evaluate(snapshot, resolver) {
		var instanceID *string
		if c.InstanceID != "" {
			id := c.InstanceID
			instanceID = &id
		}
		observed = append(observed, store.ObservedCondition{
			Kind: c.Kind.String(), InstanceID: instanceID, Detail: c.Detail,
		})
	}

	diff, err := s.DB.ReconcileConditions(ctx, observed, time.Now().UTC())
	if err != nil {
		return store.ConditionDiff{}, fmt.Errorf("reconcile conditions: %w", err)
	}
	if s.Dispatcher != nil {
		s.Dispatcher.DispatchAlerts(ctx)
	}

	if _, err := s.DB.SweepIncidents(ctx, time.Now().UTC().Add(-alerts.IncidentRetention)); err != nil {
		slog.WarnContext(ctx, "sweep incidents", slog.Any("error", err))
	}
	return diff, nil
}

// snapshot gathers everything the evaluator reads, plus the threshold resolver built from
// the rules.
func (s *Scanner) snapshot(ctx context.Context) (*alerts.Snapshot, alerts.Resolver, error) {
	instances, err := s.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("read instances: %w", err)
	}
	latest, err := s.DB.LatestTerminalJobPerInstanceKind(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read terminal jobs: %w", err)
	}
	clean, err := s.DB.LatestCleanSignalPerInstance(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read clean signals: %w", err)
	}
	running, err := s.DB.RunningJobs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read running jobs: %w", err)
	}
	lastBackups, err := s.DB.LastBackupTimes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read last backup times: %w", err)
	}
	flagged, err := s.DB.FlaggedLatest(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read flagged backups: %w", err)
	}
	schedules, err := s.DB.ListSchedules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read schedules: %w", err)
	}
	rules, err := s.DB.ListAlertRules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read alert rules: %w", err)
	}
	now := time.Now().UTC()
	incidents, err := s.DB.RecentIncidents(ctx, now.Add(-alerts.IncidentRetention))
	if err != nil {
		return nil, nil, fmt.Errorf("read recent incidents: %w", err)
	}

	var observedBuild instance.PublicBuild
	if _, err := s.DB.KVGet(ctx, instance.PublicBuildKey, &observedBuild); err != nil {
		return nil, nil, fmt.Errorf("read the observed build: %w", err)
	}

	free, err := instance.FreeSpace(s.DataRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read free space: %w", err)
	}

	return &alerts.Snapshot{
		Instances:          instances,
		LatestTerminalJobs: latest,
		CleanSignals:       clean,
		RunningJobs:        running,
		BackupSchedules:    backupCadences(ctx, schedules, instances, latest, lastBackups, now),
		LastBackups:        lastBackups,
		FlaggedBackups:     flagged,
		Incidents:          incidents,
		FailedMods:         s.failedMods(ctx, instances, latest),
		ModUpdates:         s.modUpdates(ctx, rules, instances),
		InstalledBuilds:    instance.InstalledBuilds(instances),
		BranchBuilds:       observedBuild.BranchBuilds(),
		FreeBytes:          free,
		AlarmBytes:         s.AlarmFloor(instances),
		Now:                now,
	}, alerts.RuleResolver(rules), nil
}

// failedMods reads each modded server's loader log for the mods its latest boot refused. A log
// last written before that boot belongs to an earlier one and says nothing about it.
func (s *Scanner) failedMods(
	ctx context.Context, instances []store.Instance, latest []store.Job,
) map[string]alerts.FailedMods {
	booted := lastBoots(instances, latest)
	out := map[string]alerts.FailedMods{}
	for i := range instances {
		inst := &instances[i]
		if !inst.Modded {
			continue
		}
		load, err := instance.ReadPluginLoad(inst.DataDir, s.Patterns)
		if err != nil {
			slog.WarnContext(ctx, "read the mod loader log",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
			continue
		}
		if load == nil || len(load.Failed) == 0 || load.ObservedAt.Before(booted[inst.ID]) {
			continue
		}
		var f alerts.FailedMods
		for _, p := range load.Failed {
			name := p.Name
			if p.Skipped {
				name += " (skipped)"
			} else if f.Reason == "" {
				f.Reason = p.Reason
			}
			f.Names = append(f.Names, name)
		}
		out[inst.ID] = f
	}
	return out
}

// lastBoots is when each instance last booted: its container's observed start, or the start of
// its latest successful start or restart, whichever is later. The observer sees a start only on
// its next pass, the job at once.
func lastBoots(instances []store.Instance, latest []store.Job) map[string]time.Time {
	out := make(map[string]time.Time, len(instances))
	for i := range instances {
		if at := instances[i].ContainerStartedAt; at != nil {
			out[instances[i].ID] = *at
		}
	}
	for i := range latest {
		j := &latest[i]
		started := j.Kind == jobs.KindStart.String() || j.Kind == jobs.KindRestart.String()
		if !started || j.Status != jobs.StatusSucceeded || j.InstanceID == nil || j.StartedAt == nil {
			continue
		}
		if j.StartedAt.After(out[*j.InstanceID]) {
			out[*j.InstanceID] = *j.StartedAt
		}
	}
	return out
}

// modUpdates lists the mod updates of each server an enabled mod_updates rule covers. Servers no
// rule asks about are not looked at, so the condition exists only where someone wants it.
func (s *Scanner) modUpdates(
	ctx context.Context, rules []store.AlertRule, instances []store.Instance,
) map[string][]string {
	if s.ModUpdates == nil {
		return nil
	}
	out := map[string][]string{}
	for i := range instances {
		id := instances[i].ID
		if !covered(rules, alerts.KindModUpdates, id) {
			continue
		}
		targets, err := s.ModUpdates(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "read pending mod updates", slog.String("instance_id", id), slog.Any("error", err))
			continue
		}
		for _, t := range targets {
			out[id] = append(out[id], t.FullName+" "+t.FromVersion+" → "+t.Version)
		}
	}
	return out
}

// covered reports whether an enabled rule for kind covers instanceID.
func covered(rules []store.AlertRule, kind alerts.Kind, instanceID string) bool {
	condition := &store.AlertCondition{Kind: kind.String(), InstanceID: &instanceID}
	for i := range rules {
		if alerts.Matches(&rules[i], condition) {
			return true
		}
	}
	return false
}

// backupCadences reduces each enabled schedule that takes archives to the interval the
// staleness rule needs: a backup schedule, or a restart schedule on an instance that archives
// on restart and whose latest restart ran. A restart is skipped while the server is stopped,
// and a skipped restart owes no archive. An expression this build cannot parse is skipped, as
// the clock itself skips it.
func backupCadences(
	ctx context.Context, schedules []store.Schedule, instances []store.Instance,
	latest []store.Job, lastBackups map[string]time.Time, now time.Time,
) []alerts.BackupSchedule {
	archivesOnRestart := archivingRestarts(instances, latest)
	out := make([]alerts.BackupSchedule, 0, len(schedules))
	for i := range schedules {
		sc := &schedules[i]
		if !sc.Enabled || sc.InstanceID == nil {
			continue
		}
		archives := sc.Kind == jobs.KindBackup.String() ||
			sc.Kind == jobs.KindRestart.String() && archivesOnRestart[*sc.InstanceID]
		if !archives {
			continue
		}
		every, err := scheduler.Interval(sc.Cron, now)
		if err != nil {
			slog.WarnContext(ctx, "a schedule that takes backups has an expression the panel cannot read",
				slog.String("schedule_id", sc.ID), slog.String("cron", sc.Cron))
			continue
		}
		var missed int
		if last, ok := lastBackups[*sc.InstanceID]; ok {
			missed = firesBetween(sc.Cron, sc.Timezone, last, now)
		}
		out = append(out, alerts.BackupSchedule{
			InstanceID: *sc.InstanceID, Interval: every, LastRunAt: sc.LastRunAt, Missed: missed,
		})
	}
	return out
}

// archivingRestarts is the set of instances that archive on restart and whose latest restart
// succeeded.
func archivingRestarts(instances []store.Instance, latest []store.Job) map[string]bool {
	restarted := make(map[string]bool, len(latest))
	for i := range latest {
		j := &latest[i]
		if j.InstanceID != nil && j.Kind == jobs.KindRestart.String() && j.Status == jobs.StatusSucceeded {
			restarted[*j.InstanceID] = true
		}
	}
	out := make(map[string]bool, len(instances))
	for i := range instances {
		out[instances[i].ID] = instances[i].BackupOnRestart && restarted[instances[i].ID]
	}
	return out
}

// maxCountedFires bounds firesBetween; staleness only needs to know the count passed a small
// factor.
const maxCountedFires = 100

// firesBetween counts the times expr fires on zone's clock after from and up to to.
func firesBetween(expr, zone string, from, to time.Time) int {
	n := 0
	for at := from; n < maxCountedFires; n++ {
		next, err := scheduler.NextIn(expr, zone, at)
		if err != nil || next.After(to) {
			return n
		}
		at = next
	}
	return n
}
