package api

import (
	"context"
	"errors"
	"net/http"
	"os"
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
	// rollbackWorldField is the rollback request's field that restores the linked world too.
	rollbackWorldField = "world"
)

type setupSaveRequest struct {
	Name          string `json:"name"`
	WorldBackupID string `json:"world_backup_id,omitempty"`
	// Light keeps only the packages their registry archives cannot reproduce.
	Light bool `json:"light,omitempty"`
	// BackUpWorld takes a world backup during the save and links it, in place of
	// WorldBackupID. The server must be stopped.
	BackUpWorld bool `json:"back_up_world,omitempty"`
	// LinkNextWorldBackup saves the setup waiting for the world backup the next mod change
	// takes, in place of the other two.
	LinkNextWorldBackup bool `json:"link_next_world_backup,omitempty"`
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

// setupSettled checks a setup save can read inst: stopped, or running with no operation open.
func (h *Instances) setupSettled(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if !checkInstanceState(w, r, inst, jobs.KindSetupSave) {
		return false
	}
	if instance.State(inst.State) == instance.StateRunning {
		return operationSettled(w, r, h.DB, inst.ID)
	}
	return h.setupStopped(w, r, inst)
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
	if !ok || !h.setupSettled(w, r, inst) {
		return
	}
	var req setupSaveRequest
	if err := Decode(r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := h.checkSetupSave(r.Context(), u, inst, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}

	staging, err := control.NewSetupStaging(h.Cfg.Data.Root)
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
		Light: req.Light, BackUpWorld: req.BackUpWorld, LinkNextWorld: req.LinkNextWorldBackup,
	}
	if req.BackUpWorld {
		archiveID := store.NewID()
		payload.ArchivePlan = control.ArchivePlan{
			ArchiveID: archiveID, ArchivePath: control.ArchivePath(h.Cfg.Data.Root, inst, archiveID),
		}
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
				"setup_id":               payload.SetupID,
				setupNameField:           req.Name,
				"world_backup_id":        req.WorldBackupID,
				"light":                  req.Light,
				"back_up_world":          req.BackUpWorld,
				"link_next_world_backup": req.LinkNextWorldBackup,
			},
		),
		OnClaim: control.SetupClaim(id, instance.State(inst.State)),
	}, h.ctl.SetupJobs.RunSave(inst, &payload, u.ID))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// checkSetupSave trims req's name and checks the request: a name of the right length, at most
// one way to link a world backup, a stopped server and the backup permission for taking one, and
// a linkable backup.
func (h *Instances) checkSetupSave(
	ctx context.Context,
	u *store.User,
	inst *store.Instance,
	req *setupSaveRequest,
) error {
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "" || len(req.Name) > control.MaxSetupNameBytes:
		return apierr.New(errcode.ValidationFailed).With("field", setupNameField)
	case req.LinkNextWorldBackup && (req.BackUpWorld || req.WorldBackupID != ""):
		return apierr.New(errcode.ValidationFailed).With("field", "link_next_world_backup")
	case req.BackUpWorld && req.WorldBackupID != "":
		return apierr.New(errcode.ValidationFailed).With("field", "back_up_world")
	case req.BackUpWorld && inst.State != string(instance.StateStopped):
		return apierr.New(errcode.ValidationFailed).With("field", "back_up_world")
	case req.BackUpWorld && !h.Authz.Can(ctx, u, authz.BackupsCreate, inst.ID):
		return apierr.New(errcode.Forbidden)
	case req.WorldBackupID != "":
		return h.validateSetupBackup(ctx, inst.ID, req.WorldBackupID)
	}
	return nil
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
	if _, err := os.Stat(b.Path); err != nil { //nolint:gosec // the path is the backup catalogue's own
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
	preview, err := h.setupPreview(r.Context(), inst, row, refs, false)
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
	job, err := h.ctl.SetupJobs.SubmitRestore(r.Context(), &control.SetupRestoreSubmission{
		Instance: inst, Setup: row, Refs: refs, ETag: preview.ETag, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.setups.restore",
			map[string]any{"setup_id": row.ID, setupNameField: row.Name}),
	})
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
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

// setupRollbackRequest is what a rollback does besides restoring the setup.
type setupRollbackRequest struct {
	// World restores the world backup the setup links after the setup.
	World bool `json:"world,omitempty"`
	// Start starts the server once everything is restored.
	Start bool `json:"start,omitempty"`
	// ModsOnly restores mods and config files and keeps the current launch and backup settings,
	// which is all a caller who may not manage setups gets.
	ModsOnly bool `json:"mods_only,omitempty"`
}

// rollbackSetup handles POST /instances/{id}/setups/{sid}/rollback: one job that stops the
// server if it runs, restores the setup and, if asked, its linked world, and starts the server if
// asked. An If-Match on a stopped server must be the preview's ETag.
func (h *Instances) rollbackSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.canSeeSetups(r.Context(), u, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok || !checkInstanceState(w, r, inst, jobs.KindSetupRollback) || !operationSettled(w, r, h.DB, inst.ID) {
		return
	}
	var req setupRollbackRequest
	if err := Decode(r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	req.ModsOnly = req.ModsOnly || !h.Authz.Can(r.Context(), u, authz.SetupsManage, id)
	row, payload, err := h.checkRollback(r, u, inst, &req)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	job, err := h.ctl.SetupRollback.Submit(r.Context(), &control.RollbackSubmission{
		Instance: inst, Payload: *payload, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.setups.rollback", map[string]any{
			"setup_id": row.ID, setupNameField: row.Name, rollbackWorldField: req.World, "start": req.Start,
			"mods_only": req.ModsOnly,
		}),
	})
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// checkRollback checks a rollback of the setup the request names onto inst and returns that setup
// and the job's payload: the rights to stop, start and restore the world as req needs them, a
// linked world backup that is still there, a ready preview, and a current If-Match on a stopped
// server.
func (h *Instances) checkRollback(
	r *http.Request, u *store.User, inst *store.Instance, req *setupRollbackRequest,
) (*store.SavedSetup, *control.RollbackPayload, error) {
	ctx := r.Context()
	running := inst.State == string(instance.StateRunning)
	if !h.canRollBack(ctx, u, inst.ID, running, req) {
		return nil, nil, apierr.New(errcode.Forbidden)
	}
	row, refs, err := h.DB.SetupByID(ctx, inst.ID, r.PathValue("sid"))
	if err != nil {
		return nil, nil, apierr.New(errcode.Internal).Wrap(err)
	}
	if row == nil {
		return nil, nil, apierr.New(errcode.NotFound)
	}
	preview, err := h.setupPreview(ctx, inst, row, refs, req.ModsOnly)
	if err != nil {
		return nil, nil, apierr.New(errcode.Internal).Wrap(err)
	}
	if req.World && (preview.Setup.WorldBackup == nil || !preview.Setup.WorldBackup.Available) {
		return nil, nil, apierr.New(errcode.ValidationFailed).With("field", rollbackWorldField)
	}
	if !preview.Ready {
		return nil, nil, apierr.New(errcode.InvalidState).With("problems", preview.Problems)
	}
	payload := &control.RollbackPayload{SetupID: row.ID, World: req.World, Start: req.Start, ModsOnly: req.ModsOnly}
	if match := r.Header.Get("If-Match"); match != "" && !running {
		if match != preview.ETag {
			return nil, nil, apierr.New(errcode.StaleWrite)
		}
		payload.ETag = match
	}
	return row, payload, nil
}

// canRollBack reports whether u holds the rights a rollback's steps need on the instance id: to
// stop it when it runs, to start it when req starts it, and to restore backups when req restores
// the world.
func (h *Instances) canRollBack(
	ctx context.Context, u *store.User, id string, running bool, req *setupRollbackRequest,
) bool {
	return (!running || h.Authz.Can(ctx, u, authz.InstanceStop, id)) &&
		(!req.Start || h.Authz.Can(ctx, u, authz.InstanceStart, id)) &&
		(!req.World || h.Authz.Can(ctx, u, authz.BackupsRestore, id))
}
