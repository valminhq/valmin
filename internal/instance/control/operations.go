package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

const (
	OperationCreate = "create"
	OperationImport = "import"
)

// errModEngineUnavailable means a definition requests mods with no mod engine wired.
var errModEngineUnavailable = errors.New("mods requested but no mod engine is wired")

// Operations owns the durable instance-definition chain.
type Operations struct {
	DB      *store.DB
	Engine  *jobs.Engine
	Starter *Starter
	Mods    ModInstaller
	// Provisioner and Keeper retry a failed install.
	Provisioner *Provisioner
	Keeper      *crypto.Keeper
}

// ModInstaller submits the mod installs a definition chain asks for. *manager.Installer
// implements it.
type ModInstaller interface {
	SubmitInstall(
		ctx context.Context, inst *store.Instance, req manager.PackageRequest, requestedBy string,
		afterFinish func(context.Context),
	) (*store.Job, error)
}

func operationSteps(plan *OperationPlan) []OperationStep {
	steps := []OperationStep{{Kind: jobs.KindProvision.String()}}
	for _, mod := range plan.Mods {
		steps = append(steps, OperationStep{Kind: jobs.KindModInstall.String(), Ref: mod.FullName})
	}
	if len(plan.Configs) > 0 {
		steps = append(steps, OperationStep{Kind: jobs.KindConfigApply.String()})
	}
	if plan.Start {
		steps = append(steps, OperationStep{Kind: jobs.KindStart.String()})
	}
	return steps
}

// Create stores the definition before its first job is submitted.
func (o *Operations) Create(ctx context.Context, instanceID, kind, createdBy string, plan *OperationPlan) error {
	steps, err := json.Marshal(operationSteps(plan))
	if err != nil {
		return fmt.Errorf("encode operation steps: %w", err)
	}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode operation plan: %w", err)
	}
	var by *string
	if createdBy != "" {
		by = &createdBy
	}
	if err := o.DB.CreateOperation(ctx, &store.Operation{
		ID: store.NewID(), InstanceID: instanceID, Kind: kind,
		State: store.OperationRunning, Steps: string(steps), Plan: string(encodedPlan), CreatedBy: by,
	}); err != nil {
		return fmt.Errorf("persist definition operation: %w", err)
	}
	return nil
}

// DecodeOperation reads the durable steps and remaining input together.
func DecodeOperation(op *store.Operation) (steps []OperationStep, plan OperationPlan, err error) {
	if err := json.Unmarshal([]byte(op.Steps), &steps); err != nil {
		return nil, plan, fmt.Errorf("decode operation %s steps: %w", op.ID, err)
	}
	if err := json.Unmarshal([]byte(op.Plan), &plan); err != nil {
		return nil, plan, fmt.Errorf("decode operation %s plan: %w", op.ID, err)
	}
	return steps, plan, nil
}

// OnJobFinished advances a matching chain step inside the job finish transaction.
func (o *Operations) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	if fin.InstanceID == nil {
		return nil
	}
	op, err := store.TxOpenOperation(ctx, tx, *fin.InstanceID)
	if err != nil {
		return fmt.Errorf("read open operation: %w", err)
	}
	if op == nil {
		return nil
	}
	steps, plan, err := DecodeOperation(op)
	if err != nil {
		return err
	}
	if op.Cursor < 0 || op.Cursor >= len(steps) {
		return nil
	}
	step := steps[op.Cursor]
	if step.Kind != fin.Kind.String() || step.Ref != finishedOperationRef(fin) {
		return nil
	}
	if fin.Status != jobs.StatusSucceeded {
		if err := store.TxAdvanceOperation(
			ctx,
			tx,
			op.ID,
			op.Steps,
			op.Cursor,
			store.OperationInterrupted,
		); err != nil {
			return fmt.Errorf("interrupt the outstanding step: %w", err)
		}
		return nil
	}
	steps[op.Cursor].JobID = fin.ID
	encoded, err := json.Marshal(steps)
	if err != nil {
		return fmt.Errorf("encode operation %s steps: %w", op.ID, err)
	}
	cursor := op.Cursor + 1
	state := store.OperationRunning
	if cursor == len(steps) {
		state = store.OperationCompleted
	}
	if err := store.TxSetInstanceModSides(ctx, tx, op.InstanceID, plan.Sides); err != nil {
		return fmt.Errorf("record the definition's side tags: %w", err)
	}
	if err := store.TxAdvanceOperation(ctx, tx, op.ID, string(encoded), cursor, state); err != nil {
		return fmt.Errorf("record completed step: %w", err)
	}
	return nil
}

func finishedOperationRef(fin *jobs.FinishedJob) string {
	if payload, ok := fin.Payload.(manager.InstallPayload); ok {
		return payload.FullName
	}
	return ""
}

// Advance submits the next running step after the previous step settles.
func (o *Operations) Advance(ctx context.Context, instanceID string) {
	op, err := o.DB.OpenOperation(ctx, instanceID)
	if err != nil || op == nil || op.State != store.OperationRunning {
		if err != nil {
			slog.WarnContext(
				ctx,
				"read definition operation",
				slog.String("instance_id", instanceID),
				slog.Any("error", err),
			)
		}
		return
	}
	steps, plan, err := DecodeOperation(op)
	if err != nil {
		slog.ErrorContext(
			ctx,
			"definition operation unreadable",
			slog.String("instance_id", instanceID),
			slog.Any("error", err),
		)
		return
	}
	if op.Cursor >= len(steps) {
		return
	}
	inst, err := o.DB.InstanceByID(ctx, instanceID)
	if err != nil || inst == nil {
		slog.WarnContext(
			ctx,
			"definition operation: instance vanished",
			slog.String("instance_id", instanceID),
			slog.Any("error", err),
		)
		return
	}
	if _, err := o.SubmitStep(ctx, inst, steps[op.Cursor], &plan, deref(op.CreatedBy)); err != nil {
		slog.WarnContext(ctx, "definition operation step not submitted", slog.String("instance_id", instanceID),
			slog.String("step", steps[op.Cursor].Kind), slog.Any("error", err))
	}
}

// SubmitStep submits one definition step and arranges its continuation.
func (o *Operations) SubmitStep(
	ctx context.Context,
	inst *store.Instance,
	step OperationStep,
	plan *OperationPlan,
	requestedBy string,
) (*store.Job, error) {
	next := func(ctx context.Context) { o.Advance(ctx, inst.ID) }
	switch step.Kind {
	case jobs.KindProvision.String():
		return o.retryInstall(ctx, inst, plan, requestedBy)
	case jobs.KindModInstall.String():
		if o.Mods == nil {
			return nil, errModEngineUnavailable
		}
		var req manager.PackageRequest
		found := false
		for _, mod := range plan.Mods {
			if mod.FullName == step.Ref {
				req, found = mod, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("operation plan has no mod %s", step.Ref)
		}
		job, err := o.Mods.SubmitInstall(ctx, inst, req, requestedBy, next)
		if err != nil {
			return nil, fmt.Errorf("submit install of %s: %w", step.Ref, err)
		}
		return job, nil
	case jobs.KindConfigApply.String():
		return submitConfigApply(ctx, o.Engine, inst, plan.Configs, plan.MergeConfigs, requestedBy, next)
	case jobs.KindStart.String():
		if inst.ContainerID == nil {
			return nil, fmt.Errorf("instance %s has no container to start", inst.ID)
		}
		return o.Starter.Submit(ctx, &StartSubmission{
			Instance: inst, ContainerID: *inst.ContainerID, RequestedBy: requestedBy,
		})
	default:
		return nil, fmt.Errorf("no chain step defined for kind %s", step.Kind)
	}
}

// retryInstall submits the install again for an instance whose install failed, claiming it from
// the state the failure left.
func (o *Operations) retryInstall(
	ctx context.Context, inst *store.Instance, plan *OperationPlan, requestedBy string,
) (*store.Job, error) {
	password, err := DecryptPassword(ctx, o.DB, o.Keeper, inst.ID)
	if err != nil {
		return nil, err
	}
	run := provisionRunFor(inst, password, plan.Start)
	run.RequestedBy = requestedBy
	job, err := o.Provisioner.Submit(ctx, run, instance.State(inst.State))
	if err != nil {
		return nil, fmt.Errorf("retry install of instance %s: %w", inst.ID, err)
	}
	return job, nil
}
