package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
)

// Checkpoints of a restore job, in order (12 §9.4). They record how far the run got; recovery
// resolves the swap from the directories themselves, which are the only durable truth about a
// rename that was interrupted halfway.
const (
	checkpointPreBackupTaken = control.RestorePreBackupTaken
	checkpointSwapped        = control.RestoreSwapped
)

// restorePayload is the job's persisted arguments (12 §4.1). It names the archive and nothing
// else: the directories recovery touches are derived from the instance row, so no path on a
// job payload can point the sweep at a tree it does not own.
type restorePayload = control.RestorePayload

// restoreBackup is POST /instances/{id}/backups/{bid}/restore (04 §3, 12 §3.1).
//
// The one job in the panel that can destroy a world. It requires `stopped`, is never
// cancellable (12 §8), never resumes the server afterwards, and parks the instance in `error`
// on any failure so a human decides what happens next (B7).
func (h *Instances) restoreBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsRestore, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// 12 §3.1's Requires column. A restore never stops a running server for the operator:
	// the world it would replace is the one players are in.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}
	b, ok := h.mustLoadBackup(w, r, id)
	if !ok {
		return
	}

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindRestore, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: restorePayload{BackupID: b.ID},
		Audit:   jobAudit(r.Context(), u.ID, id, "instances.backups.restore", restorePayload{BackupID: b.ID}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.SetStateTx(ctx, tx, id, instance.StateStopped, instance.StateRestoring)
			if err != nil {
				return fmt.Errorf("claim restore for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, (&control.Restorer{Snapshotter: h.snapshotter()}).Run(inst, b))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
