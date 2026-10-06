package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// SubmitAdoption publishes a verified orphan and its audit entry under the job claim.
func SubmitAdoption(
	ctx context.Context,
	engine *jobs.Engine,
	row *store.AdoptedInstance,
	requestedBy, auditIP string,
) (*store.Job, error) {
	detail, err := json.Marshal(map[string]any{
		"container_id": row.ContainerID, "instance_id": row.ID,
		"state": row.State, "game_build_id": row.GameBuildID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode adoption audit detail: %w", err)
	}
	job, err := engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindAdopt, LockKey: jobs.InstanceLockKey(row.ID),
		InstanceID: &row.ID, InstanceName: row.Name, RequestedBy: requestedBy,
		Payload: AdoptionPayload{ContainerID: row.ContainerID},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxAdoptInstance(ctx, tx, row); err != nil {
				return fmt.Errorf("publish adopted instance: %w", err)
			}
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: requestedBy, InstanceID: row.ID, Action: authz.InstanceAdopt.String(),
				Detail: string(detail), IP: auditIP,
			}); err != nil {
				return fmt.Errorf("write adoption audit: %w", err)
			}
			return nil
		},
	}, func(ctx context.Context, handle *jobs.Handle) jobs.Outcome {
		handle.Progress(ctx, 100, "adoption complete")
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		return nil, fmt.Errorf("submit adoption job: %w", err)
	}
	return job, nil
}
