package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

type cloneRequest struct {
	Name string `json:"name"`
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceClone, sourceID) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
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
	checkCloneName(&val, body.Name)
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
	run := &control.CloneRun{
		Source: source, Destination: destination,
		ArchiveID: archiveID, ArchivePath: archivePath, RequestedBy: u.ID,
		AuditIP: middleware.ClientIPFrom(r.Context()).String(),
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
			apierr.Write(w, r, apierr.New(errcode.InvalidState).
				With("state", "changed").With("allowed_states", []instance.State{instance.StateStopped}))
		default:
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		}
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// checkCloneName refuses an empty destination name or one carrying path characters.
func checkCloneName(val *apierr.Validation, name string) {
	if name == "" {
		val.Add("name", apierr.FieldRequired, "Name is required.")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		val.Add("name", apierr.FieldInvalid, "Name contains invalid path characters.")
	}
}

func (h *Instances) submitCloneWithPort(ctx context.Context, run *control.CloneRun) (*store.Job, error) {
	allocator := instance.NewAllocator(h.DB, h.Runtime, h.Cfg.Ports.Base, h.Cfg.Ports.Stride)
	var lastErr error
	for range maxPortAllocationAttempts {
		port, err := allocator.Allocate(ctx)
		if err != nil {
			return nil, fmt.Errorf("allocate clone destination port: %w", err)
		}
		run.Destination.BasePort = port
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

func (h *Instances) submitClone(ctx context.Context, run *control.CloneRun) (*store.Job, error) {
	sourceID, destinationID := run.Source.ID, run.Destination.ID
	detail, err := json.Marshal(map[string]string{
		"source_instance_id":      sourceID,
		"destination_instance_id": destinationID,
		"destination_name":        run.Destination.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("encode clone audit detail: %w", err)
	}
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindClone, LockKey: jobs.InstanceLockKey(destinationID),
		LockKeys:   []string{jobs.InstanceLockKey(sourceID)},
		InstanceID: &destinationID, InstanceName: run.Destination.Name,
		RequestedBy: run.RequestedBy,
		Payload:     control.ClonePayload{SourceID: sourceID, ArchiveID: run.ArchiveID, ArchivePath: run.ArchivePath},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			sourceEnvelope, err := store.TxInstancePassword(ctx, tx, sourceID)
			if err != nil {
				return fmt.Errorf("read clone source Password: %w", err)
			}
			run.Password, err = h.decryptStoredPassword(sourceID, sourceEnvelope)
			if err != nil {
				return err
			}
			envelope, err := h.Keeper.Encrypt(
				crypto.PurposeInstancePassword,
				crypto.InstancePasswordLocation(destinationID),
				[]byte(run.Password),
			)
			if err != nil {
				return fmt.Errorf("encrypt password for clone destination %s: %w", destinationID, err)
			}
			if err := store.TxCreateCloneInstance(ctx, tx, sourceID, &store.NewInstance{
				ID: destinationID, Name: run.Destination.Name, DataDir: run.Destination.DataDir,
				BasePort: run.Destination.BasePort, Password: envelope,
				CrossplayInstanceID: destinationID,
			}); err != nil {
				return fmt.Errorf("create clone Destination: %w", err)
			}
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: run.RequestedBy, InstanceID: sourceID, Action: authz.InstanceClone.String(),
				Detail: string(detail), IP: run.AuditIP,
			}); err != nil {
				return fmt.Errorf("audit clone submission: %w", err)
			}
			return nil
		},
	}, (&control.Cloner{DB: h.DB, Runtime: h.Runtime, Snapshotter: h.snapshotter(), HostRoot: h.Cfg.Data.HostRoot, Image: h.Cfg.Game.Image, Network: h.Cfg.Game.Network, StopTimeout: h.Cfg.Game.StopTimeout.Std(), ReadMods: func(ctx context.Context, inst *store.Instance) ([]store.InstanceMod, error) {
		_, mods, err := h.instanceDefinition(ctx, inst)
		return mods, err
	}}).Run(run))
	if err != nil {
		return nil, fmt.Errorf("submit clone of instance %s: %w", sourceID, err)
	}
	return job, nil
}
