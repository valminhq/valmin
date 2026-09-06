package api

import (
	"context"
	"database/sql"
	"fmt"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// pruneSpec is the prune kind's job spec: global, no instance, no payload. It applies each
// instance's own retention (02 §4.4 step 7) and holds no policy of its own, so an operator who
// changes "keep the last N" has one place to change it.
func pruneSpec(scheduleID string) *jobs.Spec {
	return &jobs.Spec{
		Kind:       jobs.KindPrune,
		LockKey:    jobs.GlobalLockKey(jobs.KindPrune),
		Payload:    struct{}{},
		ScheduleID: scheduleID,
	}
}

// runPrune sweeps every instance through the same retention helper a backup job's step 7 uses.
// Idempotent: a second run over a catalogue already inside its policy deletes nothing, which is
// what makes it one of the kinds 12 §9.4 allows to simply re-run.
func (h *Instances) runPrune(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error: fmt.Sprintf("read the instances: %v", err),
		}
	}

	// Collected across instances and deleted in one Finish transaction. The files are unlinked
	// as they are found, which is why that work happens out here (C1).
	type pruned struct {
		instanceID string
		archives   []backup.Entry
	}
	var all []pruned
	total := 0

	for i := range instances {
		inst := &instances[i]
		if ctx.Err() != nil {
			break
		}
		doomed, err := h.pruneArchives(ctx, inst, nil)
		if err != nil {
			// One unreadable catalogue or one file that will not unlink must not stop the
			// sweep: the other instances are still over their retention.
			jh.Log(fmt.Sprintf("%s: %v", inst.Name, err))
			continue
		}
		if len(doomed) == 0 {
			continue
		}
		all = append(all, pruned{instanceID: inst.ID, archives: doomed})
		total += len(doomed)
		jh.Progress(ctx, 100*(i+1)/len(instances), fmt.Sprintf("pruned %d archives", total))
	}

	return jobs.Outcome{
		Status: "succeeded",
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			for _, p := range all {
				for _, a := range p.archives {
					if err := store.TxDeleteBackup(ctx, tx, p.instanceID, a.ID); err != nil {
						return fmt.Errorf("prune archive %s: %w", a.ID, err)
					}
				}
			}
			return nil
		},
	}
}
