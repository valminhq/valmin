package api

import (
	"context"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// parseBackupMode reads the one query parameter. Absent means quiesced: the safe archive is
// what an operator who did not choose gets.
func parseBackupMode(r *http.Request) (control.BackupMode, error) {
	switch raw := r.URL.Query().Get("mode"); raw {
	case "", string(control.BackupQuiesced):
		return control.BackupQuiesced, nil
	case string(control.BackupHot):
		return control.BackupHot, nil
	default:
		return "", apierr.New(errcode.InvalidParameter).With("parameter", "mode")
	}
}

// createBackup is POST /instances/{id}/backups (04 §3, 12 §3.1).
//
// A quiesced backup of a running server stops it, archives, and starts it again — the job
// sequence of 12 §2.3, one of the two kinds permitted to stop a server implicitly (12 §3.2).
// A hot copy never stops anything and enters no transient state (B12).
func (h *Instances) createBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsCreate, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	mode, err := parseBackupMode(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindBackup) {
		return
	}

	// A hot copy of a running server needs the container; a quiesced one needs it to stop it.
	containerID := ""
	if inst.State == string(instance.StateRunning) {
		if containerID, ok = h.mustHaveContainer(w, r, inst); !ok {
			return
		}
	}

	job, err := h.submitBackup(r.Context(), inst, containerID, mode, u.ID, "")
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// submitBackup shares the domain submission path with scheduled backups.
func (h *Instances) submitBackup(
	ctx context.Context, inst *store.Instance, containerID string,
	mode control.BackupMode, requestedBy, scheduleID string,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.ctl.Backupper.Submit(ctx, &control.BackupSubmission{
		Instance: inst, ContainerID: containerID, Mode: mode,
		RequestedBy: requestedBy, ScheduleID: scheduleID,
		Audit: jobAudit(ctx, requestedBy, inst.ID, "instances.backups.create", struct{}{}),
	})
}
