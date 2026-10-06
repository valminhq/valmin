package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
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
		DataDir:    instance.DataDir(h.Cfg.Data.Root, destinationID),
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

func (h *Instances) submitClone(ctx context.Context, run *control.CloneRun) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts for the port retry and the response
	return h.ctl.Cloner.Submit(ctx, run)
}
