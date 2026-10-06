package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

const IncidentRetention = 24 * time.Hour

// Scanner gathers alert state, reconciles conditions, and dispatches transitions.
type Scanner struct {
	DB         *store.DB
	Engine     *jobs.Engine
	DataRoot   string
	AlarmFloor func([]store.Instance) uint64
	Thresholds func([]store.AlertRule) Resolver
	Dispatch   func(context.Context)
}

// Submit enqueues a global condition scan.
func (h *Scanner) Submit(ctx context.Context, scheduleID string) (*store.Job, error) {
	j, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindAlertScan, LockKey: jobs.GlobalLockKey(jobs.KindAlertScan),
		Payload: struct{}{}, ScheduleID: scheduleID,
	}, h.RunJob)
	if err != nil {
		return nil, fmt.Errorf("submit alert scan: %w", err)
	}
	return j, nil
}

// runAlertScan evaluates every condition and reconciles the stored set against it.
func (h *Scanner) RunJob(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	jh.Progress(ctx, 10, "Reading the panel's state")
	diff, err := h.Scan(ctx)
	if err != nil {
		return jobs.Outcome{Status: jobs.StatusFailed, Error: err.Error()}
	}
	jh.Progress(ctx, 100, fmt.Sprintf("%d opened, %d resolved", len(diff.Opened), len(diff.Resolved)))
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// scanAlerts is one scan: gather, evaluate, reconcile, dispatch what is owed. Reading is all
// done before the reconcile transaction opens, because the gather touches the filesystem (C1).
func (h *Scanner) Scan(ctx context.Context) (store.ConditionDiff, error) {
	snapshot, resolver, err := h.snapshot(ctx)
	if err != nil {
		return store.ConditionDiff{}, err
	}

	observed := make([]store.ObservedCondition, 0, 8)
	for _, c := range Evaluate(snapshot, resolver) {
		var instanceID *string
		if c.InstanceID != "" {
			id := c.InstanceID
			instanceID = &id
		}
		observed = append(observed, store.ObservedCondition{
			Kind: c.Kind.String(), InstanceID: instanceID, Detail: c.Detail,
		})
	}

	diff, err := h.DB.ReconcileConditions(ctx, observed, time.Now().UTC())
	if err != nil {
		return store.ConditionDiff{}, fmt.Errorf("reconcile conditions: %w", err)
	}
	h.Dispatch(ctx)

	if _, err := h.DB.SweepIncidents(ctx, time.Now().UTC().Add(-IncidentRetention)); err != nil {
		slog.WarnContext(ctx, "sweep incidents", slog.Any("error", err))
	}
	return diff, nil
}

// alertSnapshot gathers everything the evaluator reads, plus the threshold resolver built from
// the rules.
func (h *Scanner) snapshot(ctx context.Context) (*Snapshot, Resolver, error) {
	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("read instances: %w", err)
	}
	latest, err := h.DB.LatestTerminalJobPerInstanceKind(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read terminal jobs: %w", err)
	}
	clean, err := h.DB.LatestCleanSignalPerInstance(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read clean signals: %w", err)
	}
	running, err := h.DB.RunningJobs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read running jobs: %w", err)
	}
	lastBackups, err := h.DB.LastBackupTimes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read last backup times: %w", err)
	}
	schedules, err := h.DB.ListSchedules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read schedules: %w", err)
	}
	rules, err := h.DB.ListAlertRules(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read alert rules: %w", err)
	}
	now := time.Now().UTC()
	incidents, err := h.DB.RecentIncidents(ctx, now.Add(-IncidentRetention))
	if err != nil {
		return nil, nil, fmt.Errorf("read recent incidents: %w", err)
	}

	var observedBuild instance.PublicBuild
	if _, err := h.DB.KVGet(ctx, instance.PublicBuildKey, &observedBuild); err != nil {
		return nil, nil, fmt.Errorf("read the observed build: %w", err)
	}

	free, err := instance.FreeSpace(h.DataRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read free space: %w", err)
	}

	return &Snapshot{
		Instances:          instances,
		LatestTerminalJobs: latest,
		CleanSignals:       clean,
		RunningJobs:        running,
		BackupSchedules:    backupCadences(ctx, schedules, instances, latest, now),
		LastBackups:        lastBackups,
		Incidents:          incidents,
		InstalledBuilds:    InstalledBuilds(instances),
		PublicBuild:        observedBuild.BuildID,
		FreeBytes:          free,
		AlarmBytes:         h.AlarmFloor(instances),
		Now:                now,
	}, h.Thresholds(rules), nil
}

// backupCadences reduces each enabled schedule that takes archives to the interval the
// staleness rule needs: a backup schedule, or a restart schedule on an instance that archives
// on restart and whose latest restart ran. A restart is skipped while the server is stopped,
// and a skipped restart owes no archive. An expression this build cannot parse is skipped, as
// the clock itself skips it.
func backupCadences(
	ctx context.Context, schedules []store.Schedule, instances []store.Instance,
	latest []store.Job, now time.Time,
) []BackupSchedule {
	restarted := make(map[string]bool, len(latest))
	for i := range latest {
		j := &latest[i]
		if j.Kind == jobs.KindRestart.String() && j.Status == jobs.StatusSucceeded {
			restarted[deref(j.InstanceID)] = true
		}
	}
	archivesOnRestart := make(map[string]bool, len(instances))
	for i := range instances {
		archivesOnRestart[instances[i].ID] = instances[i].BackupOnRestart && restarted[instances[i].ID]
	}
	out := make([]BackupSchedule, 0, len(schedules))
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
		out = append(out, BackupSchedule{
			InstanceID: *sc.InstanceID, Interval: every, LastRunAt: sc.LastRunAt,
		})
	}
	return out
}

// installedBuilds reads what each instance actually runs. The manifest under server/ is the
// truth and the column only a cache of it, which a recovered game update can leave behind.
func InstalledBuilds(instances []store.Instance) map[string]string {
	out := make(map[string]string, len(instances))
	for i := range instances {
		inst := &instances[i]
		installed, err := instance.InstalledBuildID(inst.DataDir)
		if err != nil {
			installed = deref(inst.GameBuildID)
		}
		out[inst.ID] = installed
	}
	return out
}
