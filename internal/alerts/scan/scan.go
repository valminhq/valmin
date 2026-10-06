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
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// Dispatcher sends the notifications owed for stored alert edges.
type Dispatcher interface {
	DispatchAlerts(ctx context.Context)
}

// Scanner gathers alert state, reconciles conditions, and dispatches transitions. A nil
// Dispatcher reconciles without notifying.
type Scanner struct {
	DB         *store.DB
	Engine     *jobs.Engine
	DataRoot   string
	AlarmFloor func([]store.Instance) uint64
	Dispatcher Dispatcher
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
		BackupSchedules:    backupCadences(ctx, schedules, instances, latest, now),
		LastBackups:        lastBackups,
		Incidents:          incidents,
		InstalledBuilds:    instance.InstalledBuilds(instances),
		PublicBuild:        observedBuild.BuildID,
		FreeBytes:          free,
		AlarmBytes:         s.AlarmFloor(instances),
		Now:                now,
	}, alerts.RuleResolver(rules), nil
}

// backupCadences reduces each enabled schedule that takes archives to the interval the
// staleness rule needs: a backup schedule, or a restart schedule on an instance that archives
// on restart and whose latest restart ran. A restart is skipped while the server is stopped,
// and a skipped restart owes no archive. An expression this build cannot parse is skipped, as
// the clock itself skips it.
func backupCadences(
	ctx context.Context, schedules []store.Schedule, instances []store.Instance,
	latest []store.Job, now time.Time,
) []alerts.BackupSchedule {
	restarted := make(map[string]bool, len(latest))
	for i := range latest {
		j := &latest[i]
		if j.Kind == jobs.KindRestart.String() && j.Status == jobs.StatusSucceeded {
			if j.InstanceID != nil {
				restarted[*j.InstanceID] = true
			}
		}
	}
	archivesOnRestart := make(map[string]bool, len(instances))
	for i := range instances {
		archivesOnRestart[instances[i].ID] = instances[i].BackupOnRestart && restarted[instances[i].ID]
	}
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
		out = append(out, alerts.BackupSchedule{
			InstanceID: *sc.InstanceID, Interval: every, LastRunAt: sc.LastRunAt,
		})
	}
	return out
}
