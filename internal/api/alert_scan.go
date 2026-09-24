package api

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

// incidentRetention bounds how far back a crash-loop window may reach, and so how long an
// incident row is worth keeping.
const incidentRetention = 24 * time.Hour

func alertScanSpec(scheduleID string) *jobs.Spec {
	return &jobs.Spec{
		Kind:       jobs.KindAlertScan,
		LockKey:    jobs.GlobalLockKey(jobs.KindAlertScan),
		Payload:    struct{}{},
		ScheduleID: scheduleID,
	}
}

// runAlertScan evaluates every condition and reconciles the stored set against it.
func (h *Instances) runAlertScan(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	jh.Progress(ctx, 10, "Reading the panel's state")
	diff, err := h.scanAlerts(ctx)
	if err != nil {
		return jobs.Outcome{Status: jobs.StatusFailed, Error: err.Error()}
	}
	jh.Progress(ctx, 100, fmt.Sprintf("%d opened, %d resolved", len(diff.Opened), len(diff.Resolved)))
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// scanAlerts is one scan: gather, evaluate, reconcile, dispatch the edges. Reading is all done
// before the reconcile transaction opens, because the gather touches the filesystem (C1).
func (h *Instances) scanAlerts(ctx context.Context) (store.ConditionDiff, error) {
	snapshot, resolver, err := h.alertSnapshot(ctx)
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

	diff, err := h.DB.ReconcileConditions(ctx, observed, time.Now().UTC())
	if err != nil {
		return store.ConditionDiff{}, fmt.Errorf("reconcile conditions: %w", err)
	}
	h.dispatchAlerts(ctx, diff)

	if _, err := h.DB.SweepIncidents(ctx, time.Now().UTC().Add(-incidentRetention)); err != nil {
		slog.WarnContext(ctx, "sweep incidents", slog.Any("error", err))
	}
	return diff, nil
}

// alertSnapshot gathers everything the evaluator reads, plus the threshold resolver built from
// the rules.
func (h *Instances) alertSnapshot(ctx context.Context) (*alerts.Snapshot, alerts.Resolver, error) {
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
	incidents, err := h.DB.RecentIncidents(ctx, now.Add(-incidentRetention))
	if err != nil {
		return nil, nil, fmt.Errorf("read recent incidents: %w", err)
	}

	var observedBuild publicBuild
	if _, err := h.DB.KVGet(ctx, publicBuildKey, &observedBuild); err != nil {
		return nil, nil, fmt.Errorf("read the observed build: %w", err)
	}

	free, err := instance.FreeSpace(h.Cfg.Data.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("read free space: %w", err)
	}

	return &alerts.Snapshot{
		Instances:          instances,
		LatestTerminalJobs: latest,
		CleanSignals:       clean,
		RunningJobs:        running,
		BackupSchedules:    backupCadences(ctx, schedules, now),
		LastBackups:        lastBackups,
		Incidents:          incidents,
		InstalledBuilds:    installedBuilds(instances),
		PublicBuild:        observedBuild.BuildID,
		FreeBytes:          free,
		AlarmBytes:         h.reportedAlarmFloor(instances),
		Now:                now,
	}, thresholds(rules), nil
}

// backupCadences reduces each enabled backup schedule to the interval the staleness rule needs.
// An expression this build cannot parse is skipped, as the clock itself skips it.
func backupCadences(ctx context.Context, schedules []store.Schedule, now time.Time) []alerts.BackupSchedule {
	out := make([]alerts.BackupSchedule, 0, len(schedules))
	for i := range schedules {
		sc := &schedules[i]
		if sc.Kind != jobs.KindBackup.String() || !sc.Enabled || sc.InstanceID == nil {
			continue
		}
		every, err := scheduler.Interval(sc.Cron, now)
		if err != nil {
			slog.WarnContext(ctx, "backup schedule has an expression the panel cannot read",
				slog.String("schedule_id", sc.ID), slog.String("cron", sc.Cron))
			continue
		}
		out = append(out, alerts.BackupSchedule{
			InstanceID: *sc.InstanceID, Interval: every, LastRunAt: sc.LastRunAt,
		})
	}
	return out
}

// installedBuilds reads what each instance actually runs. The manifest under server/ is the
// truth and the column only a cache of it, which a recovered game update can leave behind.
func installedBuilds(instances []store.Instance) map[string]string {
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

// reportedAlarmFloor is the highest floor any running server reports, or the configured one.
// Highest, because a panel alarming below the point a server stopped saving is worse than none.
func (h *Instances) reportedAlarmFloor(instances []store.Instance) uint64 {
	var highest *instance.DiskThresholds
	for i := range instances {
		reader := h.Streams.Reader(instances[i].ID)
		if reader == nil {
			continue
		}
		reported := reader.Disk()
		if reported == nil {
			continue
		}
		if highest == nil || reported.BlockedBelowBytes > highest.BlockedBelowBytes {
			highest = reported
		}
	}
	return alarmFloor(h.Cfg.Data.FreeSpaceFloorBytes, highest)
}

// thresholds resolves a kind's params for one instance. The most specific enabled rule wins, so
// a rule naming the instance overrides one covering all of them.
func thresholds(rules []store.AlertRule) alerts.Resolver {
	return func(kind alerts.Kind, instanceID string) alerts.Params {
		var chosen *store.AlertRule
		for i := range rules {
			r := &rules[i]
			if !r.Enabled || r.ConditionKind != kind.String() {
				continue
			}
			if r.InstanceID != nil && *r.InstanceID != instanceID {
				continue
			}
			if chosen == nil || (chosen.InstanceID == nil && r.InstanceID != nil) {
				chosen = r
			}
		}
		if chosen == nil {
			return alerts.Params{}
		}
		return decodeParams(chosen.Params)
	}
}
