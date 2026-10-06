package control

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// BackupSubmission carries the inputs recorded when a backup job is queued.
type BackupSubmission struct {
	Instance    *store.Instance
	ContainerID string
	Mode        BackupMode
	RequestedBy string
	ScheduleID  string
	Audit       *store.AuditEntry
}

// Submit queues a backup with its state claim and durable resume intent.
func (b *Backupper) Submit(ctx context.Context, input *BackupSubmission) (*store.Job, error) {
	id := input.Instance.ID
	wasRunning := input.Instance.State == string(instance.StateRunning)
	backupID := store.NewID()
	dest := ArchivePath(b.DataRoot, input.Instance, backupID)
	quiescing := input.Mode == BackupMode("quiesced") && wasRunning
	trigger := store.TriggerManual
	if input.ScheduleID != "" {
		trigger = store.TriggerScheduled
	}
	job, err := b.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindBackup, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name,
		RequestedBy: input.RequestedBy, ScheduleID: input.ScheduleID,
		Payload: BackupPayload{Mode: input.Mode, Dest: dest}, Audit: input.Audit,
		ResumeAfter: quiescing,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if !quiescing {
				return nil
			}
			ok, err := setStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
			if err != nil {
				return fmt.Errorf("claim backup for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, b.Run(input.Instance, input.ContainerID, input.Mode, backupID, dest, trigger, wasRunning))
	if err != nil {
		return nil, fmt.Errorf("submit backup for instance %s: %w", id, err)
	}
	return job, nil
}

// RestartSubmission carries the inputs recorded when a restart job is queued.
type RestartSubmission struct {
	Instance    *store.Instance
	ContainerID string
	RequestedBy string
	ScheduleID  string
	Audit       *store.AuditEntry
}

// Submit queues a restart with its running-to-stopping claim.
func (r *Restarter) Submit(ctx context.Context, engine *jobs.Engine, input RestartSubmission) (*store.Job, error) {
	id := input.Instance.ID
	job, err := engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindRestart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name,
		RequestedBy: input.RequestedBy, ScheduleID: input.ScheduleID,
		Payload: struct{}{}, Audit: input.Audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := setStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
			if err != nil {
				return fmt.Errorf("claim restart for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in running state at claim", id)
			}
			return nil
		},
	}, r.Run(input.Instance, input.ContainerID))
	if err != nil {
		return nil, fmt.Errorf("submit restart for instance %s: %w", id, err)
	}
	return job, nil
}

// GameUpdateSubmission carries the inputs recorded when a game update is queued.
type GameUpdateSubmission struct {
	Instance    *store.Instance
	Confirmed   bool
	RequestedBy string
	ScheduleID  string
	Audit       *store.AuditEntry
}

// Submit queues a game update after checking mod confirmation.
func (g *GameUpdater) Submit(ctx context.Context, engine *jobs.Engine, input GameUpdateSubmission) (*store.Job, error) {
	if err := ConfirmModded(input.Instance, input.Confirmed); err != nil {
		return nil, err
	}
	id := input.Instance.ID
	job, err := engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindGameUpdate, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name,
		RequestedBy: input.RequestedBy, ScheduleID: input.ScheduleID,
		Payload: GameUpdatePayload{ConfirmModded: input.Confirmed}, Audit: input.Audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := setStateTx(ctx, tx, id, instance.StateStopped, instance.StateUpdating)
			if err != nil {
				return fmt.Errorf("claim game update for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, g.Run(input.Instance))
	if err != nil {
		return nil, fmt.Errorf("submit game update for instance %s: %w", id, err)
	}
	return job, nil
}
