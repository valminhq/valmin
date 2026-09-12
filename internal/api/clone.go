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

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

type cloneRequest struct {
	Name string `json:"name"`
}

type clonePayload struct {
	SourceID    string `json:"source_instance_id"`
	ArchiveID   string `json:"archive_id"`
	ArchivePath string `json:"archive_path"`
}

type cloneRun struct {
	source      *store.Instance
	destination *store.Instance
	password    string
	archiveID   string
	archivePath string
	requestedBy string
	auditIP     string
}

func cloneCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case "", "dirs_created", "server_cloned", "world_archived", "world_restored":
		return true, ""
	default:
		return false, "container_created"
	}
}

// clone handles a stopped source only. The claim creates the destination while holding both
// instance locks, so neither the observer nor another job can see a partial snapshot boundary.
func (h *Instances) clone(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	sourceID := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, sourceID) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceClone, sourceID) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	source, ok := h.mustLoadInstance(w, r, sourceID)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, source, jobs.KindClone) {
		return
	}
	var body cloneRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var val apierr.Validation
	if body.Name == "" {
		val.Add("name", apierr.FieldRequired, "Name is required.")
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	destinationID := store.NewID()

	destination := &store.Instance{
		ID: destinationID, Name: body.Name,
		State:      string(instance.StateProvisioning),
		DataDir:    h.localDataDir(destinationID),
		ServerName: source.ServerName, WorldName: source.WorldName,
		Public: source.Public, Crossplay: source.Crossplay, CrossplayInstanceID: destinationID,
		Preset: source.Preset, Modifiers: source.Modifiers, ExtraArgs: source.ExtraArgs,
		Modded: source.Modded, BepInExVersion: source.BepInExVersion,
		MemLimitMB: source.MemLimitMB, CPULimit: source.CPULimit, GameBuildID: source.GameBuildID,
		BackupKeepCold: source.BackupKeepCold, BackupKeepHot: source.BackupKeepHot,
		BackupOnRestart: source.BackupOnRestart,
	}
	archiveID := store.NewID()
	archivePath := filepath.Join(instance.BackupsDir(h.Cfg.Data.Root), destinationID,
		backup.Name(body.Name, "clone", archiveID))
	run := &cloneRun{
		source: source, destination: destination,
		archiveID: archiveID, archivePath: archivePath, requestedBy: u.ID,
		auditIP: middleware.ClientIPFrom(r.Context()).String(),
	}
	job, err := h.submitCloneWithPort(r.Context(), run)
	if err != nil {
		var conflict *store.JobConflict
		switch {
		case errors.As(err, &conflict):
			writeJobSubmitError(w, r, err)
		case errors.Is(err, store.ErrInstanceNameTaken), errors.Is(err, store.ErrBasePortTaken):
			writeCreateInstanceError(w, r, err)
		case errors.Is(err, store.ErrInstanceNotStopped):
			apierr.Write(w, r, apierr.New(apierr.InvalidState).
				With("state", "changed").With("allowed_states", []instance.State{instance.StateStopped}))
		default:
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		}
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

func (h *Instances) submitCloneWithPort(ctx context.Context, run *cloneRun) (*store.Job, error) {
	allocator := instance.NewAllocator(h.DB, h.Cfg.Ports.Base, h.Cfg.Ports.Stride)
	var lastErr error
	for range maxPortAllocationAttempts {
		port, err := allocator.Allocate(ctx)
		if err != nil {
			return nil, fmt.Errorf("allocate clone destination port: %w", err)
		}
		run.destination.BasePort = port
		job, err := h.submitClone(ctx, run)
		if err == nil {
			return job, nil
		}
		if !errors.Is(err, store.ErrBasePortTaken) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (h *Instances) decryptPassword(ctx context.Context, instanceID string) (string, error) {
	envelope, err := h.DB.InstancePassword(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("read encrypted password for instance %s: %w", instanceID, err)
	}
	return h.decryptStoredPassword(instanceID, envelope)
}

func (h *Instances) decryptStoredPassword(instanceID, envelope string) (string, error) {
	plaintext, err := h.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(instanceID), envelope)
	if err != nil {
		return "", fmt.Errorf("decrypt password for instance %s: %w", instanceID, err)
	}
	return string(plaintext), nil
}

func (h *Instances) submitClone(ctx context.Context, run *cloneRun) (*store.Job, error) {
	sourceID, destinationID := run.source.ID, run.destination.ID
	detail, err := json.Marshal(map[string]string{
		"source_instance_id":      sourceID,
		"destination_instance_id": destinationID,
		"destination_name":        run.destination.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("encode clone audit detail: %w", err)
	}
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindClone, LockKey: jobs.InstanceLockKey(destinationID),
		LockKeys:   []string{jobs.InstanceLockKey(sourceID)},
		InstanceID: &destinationID, InstanceName: run.destination.Name,
		RequestedBy: run.requestedBy,
		Payload:     clonePayload{SourceID: sourceID, ArchiveID: run.archiveID, ArchivePath: run.archivePath},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			sourceEnvelope, err := store.TxInstancePassword(ctx, tx, sourceID)
			if err != nil {
				return fmt.Errorf("read clone source password: %w", err)
			}
			run.password, err = h.decryptStoredPassword(sourceID, sourceEnvelope)
			if err != nil {
				return err
			}
			envelope, err := h.Keeper.Encrypt(
				crypto.PurposeInstancePassword,
				crypto.InstancePasswordLocation(destinationID),
				[]byte(run.password),
			)
			if err != nil {
				return fmt.Errorf("encrypt password for clone destination %s: %w", destinationID, err)
			}
			if err := store.TxCreateCloneInstance(ctx, tx, sourceID, &store.NewInstance{
				ID: destinationID, Name: run.destination.Name, DataDir: run.destination.DataDir,
				BasePort: run.destination.BasePort, Password: envelope,
				CrossplayInstanceID: destinationID,
			}); err != nil {
				return fmt.Errorf("create clone destination: %w", err)
			}
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: run.requestedBy, InstanceID: sourceID, Action: authz.InstanceClone.String(),
				Detail: string(detail), IP: run.auditIP,
			}); err != nil {
				return fmt.Errorf("audit clone submission: %w", err)
			}
			return nil
		},
	}, h.runClone(run))
	if err != nil {
		return nil, fmt.Errorf("submit clone of instance %s: %w", sourceID, err)
	}
	return job, nil
}

func (h *Instances) runClone(run *cloneRun) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		return h.executeClone(ctx, jh, run)
	}
}

func (h *Instances) executeClone(ctx context.Context, jh *jobs.Handle, run *cloneRun) jobs.Outcome {
	if err := instance.VerifyProcessUID(instance.WantCloneUID); err != nil {
		return cloneFailed(run.destination.ID, err)
	}
	if err := h.reloadClone(ctx, run); err != nil {
		return cloneFailed(run.destination.ID, err)
	}
	if out := cloneStep(ctx, jh, run.destination.ID, 3, "creating directories", func() error {
		return instance.EnsureInstanceDirs(run.destination.DataDir)
	}, "dirs_created"); out != nil {
		return *out
	}
	mods, out := h.cloneServerFiles(ctx, jh, run)
	if out != nil {
		return *out
	}
	archiveResult, worldPresent, out := cloneWorldFiles(ctx, jh, run)
	if out != nil {
		return *out
	}
	containerID, buildID, err := h.createCloneContainer(ctx, jh, run)
	if err != nil {
		return cloneFailed(run.destination.ID, err)
	}
	if err := jh.Checkpoint(ctx, "container_created"); err != nil {
		return cloneFailed(run.destination.ID, err)
	}

	jh.Progress(ctx, 100, "clone ready")
	return jobs.Outcome{
		Status:   jobs.StatusSucceeded,
		OnFinish: finishClone(run, mods, archiveResult, worldPresent, containerID, buildID),
	}
}

func (h *Instances) reloadClone(ctx context.Context, run *cloneRun) error {
	source, err := h.DB.InstanceByID(ctx, run.source.ID)
	if err != nil {
		return fmt.Errorf("reload clone source: %w", err)
	}
	if source == nil {
		return errors.New("clone source no longer exists")
	}
	destination, err := h.DB.InstanceByID(ctx, run.destination.ID)
	if err != nil {
		return fmt.Errorf("reload clone destination: %w", err)
	}
	if destination == nil {
		return errors.New("clone destination no longer exists")
	}
	run.source, run.destination = source, destination
	return nil
}

func (h *Instances) cloneServerFiles(
	ctx context.Context, jh *jobs.Handle, run *cloneRun,
) ([]store.InstanceMod, *jobs.Outcome) {
	jh.Progress(ctx, 8, "verifying source ownership")
	if err := instance.VerifyClonedOwnership(
		instance.ServerDir(run.source.DataDir), instance.WantCloneUID); err != nil {
		out := cloneFailed(run.destination.ID, err)
		return nil, &out
	}
	_, mods, err := h.instanceDefinition(ctx, run.source)
	if err != nil {
		out := cloneFailed(run.destination.ID, fmt.Errorf("read source mod manifest: %w", err))
		return nil, &out
	}
	for i := range mods {
		mods[i].InstanceID = run.destination.ID
	}

	jh.Progress(ctx, 12, "copying server files")
	err = instance.CloneWithProgress(ctx,
		instance.ServerDir(run.source.DataDir), instance.ServerDir(run.destination.DataDir),
		clonePollInterval, func(pct int) {
			jh.Progress(ctx, 12+pct*38/100, "copying server files")
		})
	if err != nil {
		out := cloneFailed(run.destination.ID, fmt.Errorf("copy server files: %w", err))
		return nil, &out
	}
	if err := instance.VerifyClonedOwnership(
		instance.ServerDir(run.destination.DataDir), instance.WantCloneUID); err != nil {
		out := cloneFailed(run.destination.ID, err)
		return nil, &out
	}
	return mods, cloneCheckpoint(ctx, jh, run.destination.ID, "server_cloned")
}

func cloneWorldFiles(
	ctx context.Context, jh *jobs.Handle, run *cloneRun,
) (backup.Result, bool, *jobs.Outcome) {
	jh.Progress(ctx, 55, "archiving source world")
	archiveResult, worldPresent, err := archiveCloneWorld(run)
	if err != nil {
		out := cloneFailed(run.destination.ID, err)
		return backup.Result{}, false, &out
	}
	if out := cloneCheckpoint(ctx, jh, run.destination.ID, "world_archived"); out != nil {
		return backup.Result{}, false, out
	}

	jh.Progress(ctx, 70, "restoring destination world")
	if err := restoreCloneWorld(run.archivePath, instance.WorldsDir(run.destination.DataDir)); err != nil {
		out := cloneFailed(run.destination.ID, err)
		return backup.Result{}, false, &out
	}
	if !worldPresent {
		if err := os.Remove(run.archivePath); err != nil {
			out := cloneFailed(run.destination.ID, fmt.Errorf("remove empty clone archive: %w", err))
			return backup.Result{}, false, &out
		}
	}
	return archiveResult, worldPresent, cloneCheckpoint(ctx, jh, run.destination.ID, "world_restored")
}

func (h *Instances) createCloneContainer(
	ctx context.Context, jh *jobs.Handle, run *cloneRun,
) (containerID, buildID string, err error) {
	buildID, err = instance.InstalledBuildID(run.destination.DataDir)
	if err != nil {
		return "", "", fmt.Errorf("read cloned server build: %w", err)
	}
	run.destination.GameBuildID = &buildID
	spec, err := h.cloneSpec(run)
	if err != nil {
		return "", "", err
	}
	jh.Progress(ctx, 90, "creating destination container")
	containerID, err = h.ensureInstanceContainer(ctx, spec)
	return containerID, buildID, err
}

func finishClone(
	run *cloneRun, mods []store.InstanceMod, archiveResult backup.Result, worldPresent bool,
	containerID, buildID string,
) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := finishProvisioningState(ctx, tx, run.destination.ID,
			instance.StateProvisioning, instance.StateStopped, containerID, buildID); err != nil {
			return fmt.Errorf("finish clone destination: %w", err)
		}
		if err := store.TxUpsertInstanceMods(ctx, tx, mods); err != nil {
			return fmt.Errorf("copy clone mod manifest: %w", err)
		}
		if !worldPresent {
			return nil
		}
		if err := store.TxCreateBackup(ctx, tx, &store.Backup{
			ID: run.archiveID, InstanceID: run.destination.ID, Path: archiveResult.Path,
			SizeBytes: archiveResult.SizeBytes, SHA256: archiveResult.SHA256,
			WorldName: run.source.WorldName, Trigger: store.TriggerManual, Consistent: true,
		}); err != nil {
			return fmt.Errorf("catalogue clone seed backup: %w", err)
		}
		return nil
	}
}

func cloneStep(
	ctx context.Context, jh *jobs.Handle, destinationID string, progress int, message string,
	work func() error, checkpoint string,
) *jobs.Outcome {
	jh.Progress(ctx, progress, message)
	if err := work(); err != nil {
		out := cloneFailed(destinationID, err)
		return &out
	}
	return cloneCheckpoint(ctx, jh, destinationID, checkpoint)
}

func cloneCheckpoint(
	ctx context.Context, jh *jobs.Handle, destinationID, checkpoint string,
) *jobs.Outcome {
	if err := jh.Checkpoint(ctx, checkpoint); err != nil {
		out := cloneFailed(destinationID, err)
		return &out
	}
	if jh.CancelRequested(ctx) {
		return &jobs.Outcome{Status: jobs.StatusCancelled, OnFinish: provisionOnFinishError(destinationID)}
	}
	return nil
}

func cloneFailed(destinationID string, err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(), Error: err.Error(),
		OnFinish: provisionOnFinishError(destinationID),
	}
}

func archiveCloneWorld(run *cloneRun) (backup.Result, bool, error) {
	present, err := cloneWorldPairPresent(run.source)
	if err != nil {
		return backup.Result{}, false, err
	}
	res, err := backup.Archive(instance.WorldsDir(run.source.DataDir), run.archivePath)
	if err != nil {
		return backup.Result{}, false, fmt.Errorf("archive source world: %w", err)
	}
	if present {
		if _, err := backup.Verify(res.Path, run.source.WorldName); err != nil {
			_ = os.Remove(res.Path)
			return backup.Result{}, false, fmt.Errorf("verify source world archive: %w", err)
		}
	}
	return res, present, nil
}

func cloneWorldPairPresent(inst *store.Instance) (bool, error) {
	root := filepath.Join(instance.WorldsDir(inst.DataDir), instance.WorldsLocalDir)
	found := 0
	for _, ext := range []string{worldDBExt, worldFWLExt} {
		_, err := os.Stat(filepath.Join(root, inst.WorldName+ext))
		switch {
		case err == nil:
			found++
		case errors.Is(err, os.ErrNotExist):
		default:
			return false, fmt.Errorf("inspect source world pair: %w", err)
		}
	}
	if found == 1 {
		return false, fmt.Errorf("source world is missing one file from its .db/.fwl pair")
	}
	return found == 2, nil
}

func restoreCloneWorld(archivePath, live string) error {
	if _, err := backup.RecoverSwap(live); err != nil {
		return fmt.Errorf("recover destination world swap: %w", err)
	}
	staged := live + backup.StagedSuffix
	if err := os.RemoveAll(staged); err != nil {
		return fmt.Errorf("clear destination world staging: %w", err)
	}
	if err := backup.Extract(archivePath, "", staged); err != nil {
		return fmt.Errorf("extract source world archive: %w", err)
	}
	if err := backup.Swap(live); err != nil {
		return fmt.Errorf("publish destination world: %w", err)
	}
	return nil
}

func (h *Instances) cloneSpec(run *cloneRun) (*runtime.ContainerSpec, error) {
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: run.destination.ID, DataDir: h.hostDataDir(run.destination.ID),
		BasePort: run.destination.BasePort, ServerName: run.destination.ServerName,
		WorldName: run.destination.WorldName, Password: run.password,
		Public: run.destination.Public, Crossplay: run.destination.Crossplay,
		CrossplayInstanceID: run.destination.CrossplayInstanceID,
		Preset:              deref(run.destination.Preset), Modifiers: deref(run.destination.Modifiers),
		ExtraArgs: deref(run.destination.ExtraArgs), MemLimitMB: run.destination.MemLimitMB,
		CPULimit: run.destination.CPULimit,
	}, h.Cfg.Game.Image, h.Cfg.Game.StopTimeout.Std())
	if err != nil {
		return nil, fmt.Errorf("build destination container spec: %w", err)
	}
	return spec, nil
}
