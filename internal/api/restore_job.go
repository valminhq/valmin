package api

import (
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
)

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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsRestore, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// 12 §3.1's Requires column. A restore never stops a running server for the operator:
	// the world it would replace is the one players are in.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}
	b, ok := h.mustLoadBackup(w, r, id)
	if !ok {
		return
	}

	job, err := h.ctl.Restorer.Submit(r.Context(), inst, b, u.ID,
		jobAudit(r.Context(), u.ID, id, "instances.backups.restore", control.RestorePayload{BackupID: b.ID}))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
