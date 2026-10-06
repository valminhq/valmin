package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// SetupJobs owns saved setup capture and restore work.
type SetupJobs struct {
	DB          *store.DB
	Runtime     runtime.Runtime
	DataRoot    string
	Keeper      *crypto.Keeper
	Snapshotter *Snapshotter
	Apply       func(*store.Instance, string, map[string]map[string]bool, map[string]map[string]bool) error
}

// SubmitSetupDelete removes a saved setup in the job finish transaction.
func SubmitSetupDelete(
	ctx context.Context,
	engine *jobs.Engine,
	inst *store.Instance,
	row *store.SavedSetup,
	requestedBy string,
	audit *store.AuditEntry,
) (*store.Job, error) {
	id := inst.ID
	return engine.Submit(ctx, &jobs.Spec{ //nolint:wrapcheck // preserve typed job conflicts
		Kind: jobs.KindSetupDelete, LockKey: jobs.InstanceLockKey(id),
		LockKeys:   []string{SetupArtifactLock},
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: SetupJobPayload{SetupID: row.ID}, Audit: audit,
	}, func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 100, "setup deleted")
		return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return store.TxDeleteSetup(ctx, tx, id, row.ID)
		}}
	})
}

// DecodeSetupSnapshot reads the durable snapshot attached to a saved setup.
func DecodeSetupSnapshot(row *store.SavedSetup) (SetupSnapshot, error) {
	var snap SetupSnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snap); err != nil {
		return snap, fmt.Errorf("decode saved setup %s: %w", row.ID, err)
	}
	return snap, nil
}

// ValidateSettings checks the saved launch values against the current password and world.
func (s *SetupJobs) ValidateSettings(ctx context.Context, inst *store.Instance, saved *ManifestLaunch) error {
	envelope, err := s.DB.InstancePassword(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read current server password: read encrypted password for instance %s: %w", inst.ID, err)
	}
	password, err := s.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(inst.ID),
		envelope,
	)
	if err != nil {
		return fmt.Errorf("read current server password: decrypt password for instance %s: %w", inst.ID, err)
	}
	if len(instance.ValidateLaunch(saved.ServerName, inst.WorldName, string(password))) > 0 {
		return errors.New("saved server name conflicts with the current world name or password")
	}
	if len(instance.ValidateResources(saved.MemLimitMB, saved.CPULimit)) > 0 {
		return errors.New("saved resource limits are no longer valid")
	}
	if saved.BackupKeepCold < 0 || saved.BackupKeepHot < 0 {
		return errors.New("saved backup retention is invalid")
	}
	return nil
}

// SetupStoppedClaim confirms that a setup job still owns a stopped instance at claim.
func SetupStoppedClaim(id string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
		if err != nil {
			return fmt.Errorf("claim setup job: %w", err)
		}
		if !ok {
			return store.ErrInstanceNotStopped
		}
		return nil
	}
}

// SetupFailed preserves the error code and text of a failed setup job.
func SetupFailed(err error) jobs.Outcome {
	return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
}

func (s *SetupJobs) StoppedInstance(ctx context.Context, id string) (*store.Instance, error) {
	inst, err := s.DB.InstanceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read instance for setup job: %w", err)
	}
	if inst == nil {
		return nil, errors.New("instance no longer exists")
	}
	if inst.State != string(instance.StateStopped) {
		return nil, store.ErrInstanceNotStopped
	}
	if err := AssertStopped(ctx, s.Runtime, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// RunSave captures the setup and its package bytes, checking the state again before commit.
func (s *SetupJobs) RunSave(inst *store.Instance, payload *SetupJobPayload, requestedBy string) jobs.Runner {
	state := &SetupState{DB: s.DB}
	artifacts := &SetupArtifacts{DataRoot: s.DataRoot}
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()
		fresh, err := s.StoppedInstance(ctx, inst.ID)
		if err != nil {
			return SetupFailed(err)
		}
		inst = fresh
		jh.Progress(ctx, 10, "capturing settings and managed mods")
		_, etag, err := state.Current(ctx, inst)
		if err != nil {
			return SetupFailed(err)
		}
		snap, err := state.Capture(ctx, inst)
		if err != nil {
			return SetupFailed(err)
		}
		jh.Progress(ctx, 30, "retaining package files")
		refs, err := artifacts.Save(ctx, inst, &snap, payload.StagingDir)
		if err != nil {
			return SetupFailed(err)
		}
		if err := artifacts.Stage(ctx, &snap, refs, payload.StagingDir); err != nil {
			return SetupFailed(fmt.Errorf("verify saved package files: %w", err))
		}
		fresh, err = s.StoppedInstance(ctx, inst.ID)
		if err != nil {
			return SetupFailed(err)
		}
		_, after, err := state.Current(ctx, fresh)
		if err != nil {
			return SetupFailed(err)
		}
		if after != etag {
			return SetupFailed(errors.New("server state changed while the setup was saved"))
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return SetupFailed(err)
		}
		row := &store.SavedSetup{
			ID: payload.SetupID, InstanceID: inst.ID, Name: payload.Name,
			CreatedBy: requestedBy, GameBuildID: deref(inst.GameBuildID),
			WorldName: inst.WorldName, SnapshotJSON: string(raw), BackupID: payload.BackupID,
		}
		jh.Progress(ctx, 100, "setup saved")
		return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return store.TxSaveSetup(ctx, tx, row, refs)
		}}
	}
}
