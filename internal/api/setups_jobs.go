package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const (
	setupNameField = "name"
)

type setupSaveRequest struct {
	Name          string `json:"name"`
	WorldBackupID string `json:"world_backup_id,omitempty"`
}

func setupStagingRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "staging", "setups")
}

func (h *Instances) setupStopped(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if inst.State != string(instance.StateStopped) {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return false
	}
	if !operationSettled(w, r, h.DB, inst.ID) {
		return false
	}
	if err := h.assertStopped(r.Context(), inst); err != nil {
		if errors.Is(err, instance.ErrServerRunning) {
			apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped))
		} else {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		}
		return false
	}
	return true
}

func (h *Instances) setupStaging() (string, error) {
	root := setupStagingRoot(h.Cfg.Data.Root)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("create setup staging root: %w", err)
	}
	staging, err := os.MkdirTemp(root, "job-")
	if err != nil {
		return "", fmt.Errorf("create setup staging directory: %w", err)
	}
	return staging, nil
}

func (h *Instances) saveSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok || !h.setupStopped(w, r, inst) {
		return
	}
	var req setupSaveRequest
	if err := Decode(r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		apierr.Write(w, r, apierr.New(errcode.ValidationFailed).With("field", setupNameField))
		return
	}
	if req.WorldBackupID != "" {
		if err := h.validateSetupBackup(r.Context(), inst.ID, req.WorldBackupID); err != nil {
			apierr.Write(w, r, err)
			return
		}
	}

	staging, err := h.setupStaging()
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()
	payload := control.SetupJobPayload{
		SetupID: store.NewID(), StagingDir: staging, Name: req.Name, BackupID: req.WorldBackupID,
	}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindSetupSave, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{control.SetupArtifactLock},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: payload,
		Audit: jobAudit(
			r.Context(),
			u.ID,
			id,
			"instances.setups.save",
			map[string]any{
				"setup_id":        payload.SetupID,
				setupNameField:    req.Name,
				"world_backup_id": req.WorldBackupID,
			},
		),
		OnClaim: control.SetupStoppedClaim(id),
	}, (&control.SetupJobs{DB: h.DB, Runtime: h.Runtime, DataRoot: h.Cfg.Data.Root}).RunSave(inst, &payload, u.ID))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

func (h *Instances) validateSetupBackup(ctx context.Context, instanceID, backupID string) error {
	b, err := h.DB.BackupByID(ctx, instanceID, backupID)
	if err != nil {
		return apierr.New(errcode.Internal).Wrap(err)
	}
	if b == nil {
		return apierr.New(errcode.NotFound)
	}
	if !b.Consistent {
		return apierr.New(errcode.ValidationFailed).With("field", "world_backup_id")
	}
	if _, err := os.Stat(b.Path); err != nil {
		return apierr.New(errcode.ValidationFailed).With("field", "world_backup_id").Wrap(err)
	}
	return nil
}

func (h *Instances) restoreSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok || !h.setupStopped(w, r, inst) {
		return
	}
	row, refs, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	preview, err := h.setupPreview(r.Context(), inst, row, refs)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if r.Header.Get("If-Match") != preview.ETag {
		apierr.Write(w, r, apierr.New(errcode.StaleWrite))
		return
	}
	if !preview.Ready {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).With("problems", preview.Problems))
		return
	}
	staging, err := h.setupStaging()
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()
	payload := control.SetupJobPayload{SetupID: row.ID, StagingDir: staging, ETag: preview.ETag}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindSetupRestore, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{control.SetupArtifactLock},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: payload,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.setups.restore",
			map[string]any{"setup_id": row.ID, setupNameField: row.Name}),
		OnClaim: control.SetupStoppedClaim(id),
	}, (&control.SetupJobs{
		DB: h.DB, Runtime: h.Runtime, DataRoot: h.Cfg.Data.Root, Keeper: h.Keeper,
		Snapshotter: h.snapshotter(), Apply: h.setupApply,
	}).RunRestore(inst, row, refs, &payload))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

func (h *Instances) deleteSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, _, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	job, err := control.SubmitSetupDelete(r.Context(), h.Engine, inst, row, u.ID,
		jobAudit(r.Context(), u.ID, id, "instances.setups.delete",
			map[string]any{"setup_id": row.ID, setupNameField: row.Name}))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
