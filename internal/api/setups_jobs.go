package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const (
	setupNameField    = "name"
	setupArtifactLock = "global:setup-artifacts"
)

type setupSaveRequest struct {
	Name          string `json:"name"`
	WorldBackupID string `json:"world_backup_id,omitempty"`
}

type setupJobPayload struct {
	SetupID    string `json:"setup_id"`
	StagingDir string `json:"staging_dir,omitempty"`
	ETag       string `json:"etag,omitempty"`
	Name       string `json:"name,omitempty"`
	BackupID   string `json:"world_backup_id,omitempty"`
}

func setupStagingRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "staging", "setups")
}

func (h *Instances) setupStopped(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if inst.State != string(instance.StateStopped) {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return false
	}
	if !operationSettled(w, r, h.DB, inst.ID) {
		return false
	}
	if err := h.assertStopped(r.Context(), inst); err != nil {
		if errors.Is(err, errServerRunning) {
			apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped))
		} else {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
		apierr.Write(w, r, apierr.New(apierr.ValidationFailed).With("field", setupNameField))
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
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()
	payload := setupJobPayload{
		SetupID: store.NewID(), StagingDir: staging, Name: req.Name, BackupID: req.WorldBackupID,
	}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindSetupSave, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{setupArtifactLock},
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
		OnClaim: setupStoppedClaim(id),
	}, h.runSetupSave(inst, &payload, u.ID))
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
		return apierr.New(apierr.Internal).Wrap(err)
	}
	if b == nil {
		return apierr.New(apierr.NotFound)
	}
	if !b.Consistent {
		return apierr.New(apierr.ValidationFailed).With("field", "world_backup_id")
	}
	if _, err := os.Stat(b.Path); err != nil {
		return apierr.New(apierr.ValidationFailed).With("field", "world_backup_id").Wrap(err)
	}
	return nil
}

func setupStoppedClaim(id string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := holdStateTx(ctx, tx, id, instance.StateStopped)
		if err != nil {
			return err
		}
		if !ok {
			return store.ErrInstanceNotStopped
		}
		return nil
	}
}

func setupFailed(err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(), Error: err.Error(),
	}
}

func (h *Instances) stoppedSetupInstance(ctx context.Context, id string) (*store.Instance, error) {
	inst, err := h.DB.InstanceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read instance for setup job: %w", err)
	}
	if inst == nil {
		return nil, errors.New("instance no longer exists")
	}
	if inst.State != string(instance.StateStopped) {
		return nil, store.ErrInstanceNotStopped
	}
	if err := h.assertStopped(ctx, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

func (h *Instances) runSetupSave(
	inst *store.Instance, payload *setupJobPayload, requestedBy string,
) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()
		fresh, err := h.stoppedSetupInstance(ctx, inst.ID)
		if err != nil {
			return setupFailed(err)
		}
		inst = fresh
		jh.Progress(ctx, 10, "capturing settings and managed mods")
		_, etag, err := h.currentSetupState(ctx, inst)
		if err != nil {
			return setupFailed(err)
		}
		snap, err := h.captureSetupSnapshot(ctx, inst)
		if err != nil {
			return setupFailed(err)
		}

		jh.Progress(ctx, 30, "retaining package files")
		refs, err := h.saveSetupArtifacts(ctx, inst, &snap, payload.StagingDir)
		if err != nil {
			return setupFailed(err)
		}
		if err := h.stageSetupArtifacts(ctx, &snap, refs, payload.StagingDir); err != nil {
			return setupFailed(fmt.Errorf("verify saved package files: %w", err))
		}
		fresh, err = h.stoppedSetupInstance(ctx, inst.ID)
		if err != nil {
			return setupFailed(err)
		}
		_, after, err := h.currentSetupState(ctx, fresh)
		if err != nil {
			return setupFailed(err)
		}
		if after != etag {
			return setupFailed(errors.New("server state changed while the setup was saved"))
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return setupFailed(err)
		}
		row := &store.SavedSetup{
			ID: payload.SetupID, InstanceID: inst.ID, Name: payload.Name,
			CreatedBy: requestedBy, GameBuildID: deref(inst.GameBuildID),
			WorldName: inst.WorldName, SnapshotJSON: string(raw), BackupID: payload.BackupID,
		}
		jh.Progress(ctx, 100, "setup saved")
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxSaveSetup(ctx, tx, row, refs)
			},
		}
	}
}

func (h *Instances) restoreSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok || !h.setupStopped(w, r, inst) {
		return
	}
	row, refs, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	preview, err := h.setupPreview(r.Context(), inst, row, refs)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if r.Header.Get("If-Match") != preview.ETag {
		apierr.Write(w, r, apierr.New(apierr.StaleWrite))
		return
	}
	if !preview.Ready {
		apierr.Write(w, r, apierr.New(apierr.InvalidState).With("problems", preview.Problems))
		return
	}
	staging, err := h.setupStaging()
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()
	payload := setupJobPayload{SetupID: row.ID, StagingDir: staging, ETag: preview.ETag}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindSetupRestore, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{setupArtifactLock},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: payload,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.setups.restore",
			map[string]any{"setup_id": row.ID, setupNameField: row.Name}),
		OnClaim: setupStoppedClaim(id),
	}, h.runSetupRestore(inst, row, refs, &payload))
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, _, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindSetupDelete, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{setupArtifactLock},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: setupJobPayload{SetupID: row.ID},
		Audit: jobAudit(r.Context(), u.ID, id, "instances.setups.delete",
			map[string]any{"setup_id": row.ID, setupNameField: row.Name}),
	}, func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 100, "setup deleted")
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxDeleteSetup(ctx, tx, id, row.ID)
			},
		}
	})
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
