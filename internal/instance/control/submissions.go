package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
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
	quiescing := input.Mode == BackupQuiesced && wasRunning
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
			ok, err := instance.SetStateTx(ctx, tx, id, instance.StateRunning, instance.StateStopping)
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
func (r *Restarter) Submit(ctx context.Context, input *RestartSubmission) (*store.Job, error) {
	id := input.Instance.ID
	job, err := r.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindRestart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name,
		RequestedBy: input.RequestedBy, ScheduleID: input.ScheduleID,
		Payload: struct{}{}, Audit: input.Audit,
		OnClaim: transitionClaim(jobs.KindRestart, id, instance.StateRunning, instance.StateStopping),
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
func (g *GameUpdater) Submit(ctx context.Context, input *GameUpdateSubmission) (*store.Job, error) {
	if err := ConfirmModded(input.Instance, input.Confirmed); err != nil {
		return nil, err
	}
	id := input.Instance.ID
	job, err := g.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindGameUpdate, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name,
		RequestedBy: input.RequestedBy, ScheduleID: input.ScheduleID,
		Payload: GameUpdatePayload{ConfirmModded: input.Confirmed}, Audit: input.Audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.SetStateTx(ctx, tx, id, instance.StateStopped, instance.StateUpdating)
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

// StartSubmission carries the inputs recorded when a start job is queued.
type StartSubmission struct {
	Instance    *store.Instance
	ContainerID string
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit claims stopped to starting and queues the start. The endpoint, a definition chain's
// start step and a resume intent all enter starting through this one claim.
func (s *Starter) Submit(ctx context.Context, input *StartSubmission) (*store.Job, error) {
	id := input.Instance.ID
	job, err := s.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindStart, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name, RequestedBy: input.RequestedBy,
		Payload: struct{}{}, Audit: input.Audit,
		OnClaim: transitionClaim(jobs.KindStart, id, instance.StateStopped, instance.StateStarting),
	}, s.Run(id, input.ContainerID))
	if err != nil {
		return nil, fmt.Errorf("submit start for instance %s: %w", id, err)
	}
	return job, nil
}

// StopSubmission carries the inputs recorded when a stop job is queued.
type StopSubmission struct {
	Instance    *store.Instance
	ContainerID string
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit claims running to stopping and queues the stop.
func (s *Stopper) Submit(ctx context.Context, input *StopSubmission) (*store.Job, error) {
	id := input.Instance.ID
	job, err := s.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindStop, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: input.Instance.Name, RequestedBy: input.RequestedBy,
		Payload: struct{}{}, Audit: input.Audit,
		OnClaim: transitionClaim(jobs.KindStop, id, instance.StateRunning, instance.StateStopping),
	}, s.Run(id, input.ContainerID))
	if err != nil {
		return nil, fmt.Errorf("submit stop for instance %s: %w", id, err)
	}
	return job, nil
}

// DeleteSubmission carries the inputs recorded when a delete job is queued.
type DeleteSubmission struct {
	Instance    *store.Instance
	KeepWorlds  bool
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit claims the instance's current state to deleting and queues the delete. A re-run of a
// delete whose process died starts from deleting, which the claim accepts as a hold.
func (d *Deleter) Submit(ctx context.Context, input *DeleteSubmission) (*store.Job, error) {
	inst := input.Instance
	id, from := inst.ID, instance.State(inst.State)
	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}
	job, err := d.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindDelete, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{"remote_instance:" + id},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: input.RequestedBy,
		Payload: DeletePayload{KeepWorlds: input.KeepWorlds},
		Audit:   input.Audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxCheckRemoteProtection(ctx, tx, id, ""); err != nil {
				return fmt.Errorf("remote operation: %w", err)
			}
			var ok bool
			var err error
			if from == instance.StateDeleting {
				ok, err = instance.HoldStateTx(ctx, tx, id, instance.StateDeleting)
			} else {
				ok, err = instance.SetStateTx(ctx, tx, id, from, instance.StateDeleting)
			}
			if err != nil {
				return fmt.Errorf("claim delete for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in %s state at claim", id, from)
			}
			return nil
		},
	}, d.Run(id, containerID, inst.DataDir, input.KeepWorlds))
	if err != nil {
		return nil, fmt.Errorf("submit delete for instance %s: %w", id, err)
	}
	return job, nil
}

// Submit claims from to provisioning and queues the provision. from is created for a new
// instance and provisioning for a resumed run, which the claim accepts as a hold.
func (p *Provisioner) Submit(ctx context.Context, run *ProvisionRun, from instance.State) (*store.Job, error) {
	id := run.InstanceID
	job, err := p.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindProvision, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: run.Name, RequestedBy: run.RequestedBy,
		Payload: ProvisionPayload{StartAfterProvision: run.StartAfterProvision},
		Audit:   run.Audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			var ok bool
			var err error
			if from == instance.StateProvisioning {
				ok, err = instance.HoldStateTx(ctx, tx, id, from)
			} else {
				ok, err = instance.SetStateTx(ctx, tx, id, from, instance.StateProvisioning)
			}
			if err != nil {
				return fmt.Errorf("claim provision for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in %s state at claim", id, from)
			}
			return nil
		},
	}, p.Run(run))
	if err != nil {
		return nil, fmt.Errorf("submit provision for instance %s: %w", id, err)
	}
	return job, nil
}

// Submit queues a clone. The claim re-encrypts the source's password for the destination,
// creates the destination row and audits the request in one transaction.
func (c *Cloner) Submit(ctx context.Context, run *CloneRun) (*store.Job, error) {
	sourceID, destinationID := run.Source.ID, run.Destination.ID
	detail, err := json.Marshal(map[string]string{
		"source_instance_id":      sourceID,
		"destination_instance_id": destinationID,
		"destination_name":        run.Destination.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("encode clone audit detail: %w", err)
	}
	job, err := c.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindClone, LockKey: jobs.InstanceLockKey(destinationID),
		LockKeys:   []string{jobs.InstanceLockKey(sourceID)},
		InstanceID: &destinationID, InstanceName: run.Destination.Name,
		RequestedBy: run.RequestedBy,
		Payload:     ClonePayload{SourceID: sourceID, ArchiveID: run.ArchiveID, ArchivePath: run.ArchivePath},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			sourceEnvelope, err := store.TxInstancePassword(ctx, tx, sourceID)
			if err != nil {
				return fmt.Errorf("read clone source password: %w", err)
			}
			run.Password, err = DecryptStoredPassword(c.Keeper, sourceID, sourceEnvelope)
			if err != nil {
				return fmt.Errorf("read clone source password: %w", err)
			}
			envelope, err := c.Keeper.Encrypt(
				crypto.PurposeInstancePassword,
				crypto.InstancePasswordLocation(destinationID),
				[]byte(run.Password),
			)
			if err != nil {
				return fmt.Errorf("encrypt password for clone destination %s: %w", destinationID, err)
			}
			if err := store.TxCreateCloneInstance(ctx, tx, sourceID, &store.NewInstance{
				ID: destinationID, Name: run.Destination.Name, DataDir: run.Destination.DataDir,
				BasePort: run.Destination.BasePort, Password: envelope,
				CrossplayInstanceID: destinationID,
			}); err != nil {
				return fmt.Errorf("create clone destination: %w", err)
			}
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: run.RequestedBy, InstanceID: sourceID, Action: authz.InstanceClone.String(),
				Detail: string(detail), IP: run.AuditIP,
			}); err != nil {
				return fmt.Errorf("audit clone submission: %w", err)
			}
			return nil
		},
	}, c.Run(run))
	if err != nil {
		return nil, fmt.Errorf("submit clone of instance %s: %w", sourceID, err)
	}
	return job, nil
}

// transitionClaim is an OnClaim that moves the instance from one state to another, refusing
// the job when the instance has already left from.
func transitionClaim(kind jobs.Kind, id string, from, to instance.State) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := instance.SetStateTx(ctx, tx, id, from, to)
		if err != nil {
			return fmt.Errorf("claim %s for instance %s: %w", kind, id, err)
		}
		if !ok {
			return fmt.Errorf("instance %s not in %s state at claim", id, from)
		}
		return nil
	}
}
