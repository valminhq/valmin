package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// SetupRollback puts a server back on a saved setup in one job: it stops the server if it runs,
// restores the setup, restores the setup's linked world if asked and starts the server again if
// asked, each step as the job that does it alone.
type SetupRollback struct {
	Engine   *jobs.Engine
	Chainer  *Chainer
	Setups   *SetupJobs
	Restorer *Restorer
}

// RollbackPayload is what a setup rollback job records.
type RollbackPayload struct {
	SetupID string `json:"setup_id"`
	// ETag is the fingerprint of the stopped server's state the caller previewed the restore
	// against; empty, the restore checks the server as it is once stopped.
	ETag string `json:"etag,omitempty"`
	// World restores a world backup after the setup: WorldBackupID, or the one the setup links
	// when that is empty.
	World bool `json:"world,omitempty"`
	// WorldBackupID is the backup World restores, one taken under the setup's mods.
	WorldBackupID string `json:"world_backup_id,omitempty"`
	// Start starts the server once everything is restored.
	Start bool `json:"start,omitempty"`
	// ModsOnly keeps the current launch and backup settings.
	ModsOnly bool `json:"mods_only,omitempty"`
}

// RollbackSubmission is one rollback to queue.
type RollbackSubmission struct {
	Instance    *store.Instance
	Payload     RollbackPayload
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit queues sub's rollback under the instance's chain lock, so each step's job can still
// take the instance lock.
func (r *SetupRollback) Submit(ctx context.Context, sub *RollbackSubmission) (*store.Job, error) {
	id := sub.Instance.ID
	payload := sub.Payload
	job, err := r.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindSetupRollback, LockKey: jobs.ChainLockKey(id),
		InstanceID: &id, InstanceName: sub.Instance.Name, RequestedBy: sub.RequestedBy,
		Payload: payload, Audit: sub.Audit,
	}, r.run(id, &payload, sub.RequestedBy))
	if err != nil {
		return nil, fmt.Errorf("submit setup rollback for instance %s: %w", id, err)
	}
	return job, nil
}

// run is the rollback job: its steps run in order, each as its own job, and none runs after a
// step fails.
func (r *SetupRollback) run(id string, p *RollbackPayload, requestedBy string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		inst, err := r.Chainer.fresh(ctx, id)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		if out, ok := r.chosenWorldGone(ctx, id, p); ok {
			return out
		}
		chain := trialChain(inst, requestedBy)
		chainStep := func(typ string) func(context.Context, *jobs.Handle) error {
			return func(ctx context.Context, jh *jobs.Handle) error {
				return r.Chainer.runStep(ctx, jh, chain, &ChainStep{Type: typ})
			}
		}
		restoreSetup := func(ctx context.Context, jh *jobs.Handle) error {
			return r.restoreSetup(ctx, jh, id, p, requestedBy)
		}
		restoreWorld := func(ctx context.Context, jh *jobs.Handle) error {
			return r.restoreWorld(ctx, jh, id, p, requestedBy)
		}
		didStop := instance.State(inst.State) == instance.StateRunning
		var steps []trialStep
		if didStop {
			steps = append(steps, trialStep{5, "stopping the server", "stop the server", chainStep(StepStop)})
		}
		steps = append(steps, trialStep{20, "restoring the setup", "restore the setup", restoreSetup})
		if p.World {
			steps = append(steps, trialStep{60, "restoring the world", "restore the world", restoreWorld})
		}
		if p.Start {
			steps = append(steps, trialStep{85, "starting the server", "start the server", chainStep(StepStart)})
		}
		for i, step := range steps {
			jh.Progress(ctx, step.progress, step.message)
			if err := step.run(ctx, jh); err != nil {
				if didStop && i > 0 {
					jh.Log("the server is stopped and has not been started again")
				}
				return trialFailure(ctx, err, step.failure)
			}
		}
		jh.Progress(ctx, 100, rollbackReport(p))
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

// chosenWorldGone returns the failed outcome, and true, when the world backup p names is no
// longer in instance id's catalogue: retention may have removed it since the request, and the
// rollback then changes nothing.
func (r *SetupRollback) chosenWorldGone(ctx context.Context, id string, p *RollbackPayload) (jobs.Outcome, bool) {
	if p.WorldBackupID == "" {
		return jobs.Outcome{}, false
	}
	b, err := r.Setups.DB.BackupByID(ctx, id, p.WorldBackupID)
	if err != nil {
		return jobs.Failed(errcode.Internal, err), true
	}
	if b == nil {
		return jobs.Failed(errcode.InvalidState, errors.New("the chosen world backup no longer exists")), true
	}
	return jobs.Outcome{}, false
}

// restoreSetup runs the setup restore of p on the stopped instance id as its own job.
func (r *SetupRollback) restoreSetup(
	ctx context.Context, jh *jobs.Handle, id string, p *RollbackPayload, requestedBy string,
) error {
	inst, err := r.Chainer.fresh(ctx, id)
	if err != nil {
		return err
	}
	row, refs, err := r.Setups.DB.SetupByID(ctx, id, p.SetupID)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("the setup no longer exists")
	}
	etag := p.ETag
	if etag == "" {
		if _, etag, err = (&SetupState{DB: r.Setups.DB}).Current(ctx, inst); err != nil {
			return err
		}
	}
	job, err := submitStep(ctx, jh, func() (*store.Job, error) {
		return r.Setups.SubmitRestore(ctx, &SetupRestoreSubmission{
			Instance: inst, Setup: row, Refs: refs, ETag: etag, ModsOnly: p.ModsOnly, RequestedBy: requestedBy,
		})
	})
	if err != nil {
		return err
	}
	return r.Chainer.waitChild(ctx, jh, job)
}

// restoreWorld runs the restore of p's world backup, or the one p's setup links when p names
// none, on the stopped instance id as its own job.
func (r *SetupRollback) restoreWorld(
	ctx context.Context, jh *jobs.Handle, id string, p *RollbackPayload, requestedBy string,
) error {
	inst, err := r.Chainer.fresh(ctx, id)
	if err != nil {
		return err
	}
	row, _, err := r.Setups.DB.SetupByID(ctx, id, p.SetupID)
	if err != nil {
		return err
	}
	backupID := p.WorldBackupID
	if backupID == "" && row != nil {
		backupID = row.BackupID
	}
	if backupID == "" {
		return errors.New("the setup links no world backup")
	}
	b, err := r.Setups.DB.BackupByID(ctx, id, backupID)
	if err != nil {
		return err
	}
	if b == nil {
		return errors.New("the chosen world backup no longer exists")
	}
	job, err := submitStep(ctx, jh, func() (*store.Job, error) {
		return r.Restorer.Submit(ctx, inst, b, requestedBy, nil)
	})
	if err != nil {
		return err
	}
	if err := r.Chainer.waitChild(ctx, jh, job); err != nil {
		return fmt.Errorf("%w; the setup stays restored and the server needs a check", err)
	}
	return nil
}

// maxStepConflicts is how many times a step waits for a job holding a lock it needs before the
// rollback gives up.
const maxStepConflicts = 3

// submitStep submits a step's job through submit. When another job holds a lock it needs, it
// waits for that job to end and submits again, up to maxStepConflicts times.
func submitStep(ctx context.Context, jh *jobs.Handle, submit func() (*store.Job, error)) (*store.Job, error) {
	for attempt := 0; ; attempt++ {
		job, err := submit()
		var conflict *store.JobConflict
		if !errors.As(err, &conflict) || attempt == maxStepConflicts {
			return job, err
		}
		jh.Log(fmt.Sprintf("waiting for a %s job to finish", conflict.Kind))
		if _, err := jh.WaitJob(ctx, conflict.JobID); err != nil {
			return nil, err
		}
	}
}

// rollbackReport is the job's result line.
func rollbackReport(p *RollbackPayload) string {
	parts := []string{"setup restored"}
	if p.World {
		parts = append(parts, "world restored")
	}
	if p.Start {
		parts = append(parts, "the server is running")
	} else {
		parts = append(parts, "the server is stopped")
	}
	return strings.Join(parts, "; ")
}
