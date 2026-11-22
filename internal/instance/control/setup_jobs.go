package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

const (
	// MaxSetupNameBytes bounds a saved setup's name.
	MaxSetupNameBytes = 120
	// keepAutomaticSetups is how many automatic setups each server keeps.
	keepAutomaticSetups = 3
)

// SetupJobs owns saved setup capture and restore work.
type SetupJobs struct {
	DB       *store.DB
	Runtime  runtime.Runtime
	DataRoot string
	Keeper   *crypto.Keeper
	Worlds   *Worlds
	// Archives fetches the packages a light setup does not keep; nil leaves light setups unable
	// to restore them.
	Archives PackageArchives
	// Apply replaces ApplySetupPaths, so a test can fail a restore after it placed files.
	Apply func(inst *store.Instance, staging string, current, target SetupFiles) error
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
	return engine.Submit(ctx, &jobs.Spec{
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

// DecodeSetupSnapshot reads the durable snapshot attached to a saved setup, with the config
// files row carries.
func DecodeSetupSnapshot(row *store.SavedSetup) (SetupSnapshot, error) {
	var snap SetupSnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snap); err != nil {
		return snap, fmt.Errorf("decode saved setup %s: %w", row.ID, err)
	}
	if snap.ConfigTree {
		snap.ConfigFiles, snap.ConfigsTruncated = row.Configs, row.ConfigsTruncated
	}
	return snap, nil
}

// ValidateSettings checks the saved launch values against the current password and world.
func (s *SetupJobs) ValidateSettings(ctx context.Context, inst *store.Instance, saved *ManifestLaunch) error {
	password, err := DecryptPassword(ctx, s.DB, s.Keeper, inst.ID)
	if err != nil {
		return fmt.Errorf("read current server password: %w", err)
	}
	if len(instance.ValidateLaunch(saved.ServerName, inst.WorldName, password)) > 0 {
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

func (s *SetupJobs) stoppedInstance(ctx context.Context, id string) (*store.Instance, error) {
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
	artifacts := &setupArtifacts{DataRoot: s.DataRoot, Archives: s.Archives}
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()
		fresh, err := s.stoppedInstance(ctx, inst.ID)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		inst = fresh
		jh.Progress(ctx, 10, "capturing settings and managed mods")
		snap, etag, err := state.Current(ctx, inst)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		snap.Light = payload.Light
		jh.Progress(ctx, 30, "retaining package and config files")
		refs, err := s.retainSetup(ctx, inst, &snap, payload.StagingDir)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		if err := artifacts.stage(ctx, &snap, refs, payload.StagingDir); err != nil {
			return jobs.Failed(errcode.Internal, fmt.Errorf("verify saved package files: %w", err))
		}
		fresh, err = s.stoppedInstance(ctx, inst.ID)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		_, after, err := state.Current(ctx, fresh)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		if after != etag {
			return jobs.Failed(errcode.Internal, errors.New("server state changed while the setup was saved"))
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		row := &store.SavedSetup{
			ID: payload.SetupID, InstanceID: inst.ID, Name: payload.Name,
			CreatedBy: requestedBy, GameBuildID: deref(inst.GameBuildID),
			WorldName: inst.WorldName, SnapshotJSON: string(raw), BackupID: payload.BackupID,
			Configs: snap.ConfigFiles, ConfigsTruncated: snap.ConfigsTruncated,
		}
		jh.Progress(ctx, 100, "setup saved")
		return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			return store.TxSaveSetup(ctx, tx, row, refs)
		}}
	}
}

// UnavailableArchives names each package of a light setup that it does not keep, has files to
// restore, and can no longer be fetched: its registry is disabled, or it is neither cached nor
// listed.
func (s *SetupJobs) UnavailableArchives(
	ctx context.Context, snap *SetupSnapshot, refs []store.SetupArtifactRef,
) ([]string, error) {
	if !snap.Light {
		return nil, nil
	}
	kept := make(map[string]bool, len(refs))
	for _, ref := range refs {
		kept[ref.FullName] = true
	}
	var missing []string
	for _, mod := range snap.Mods {
		entries, err := setupManifest(&mod)
		if err != nil {
			return nil, err
		}
		if kept[mod.FullName] || len(setupPayloadEntries(entries)) == 0 {
			continue
		}
		available := false
		if src, ok := source.ByName(mod.Source); ok && s.Archives != nil {
			var err error
			if available, err = s.Archives.ArchiveAvailable(ctx, mod.FullName, mod.Version, src); err != nil {
				return nil, err
			}
		}
		if !available {
			missing = append(missing, mod.FullName+" "+mod.Version)
		}
	}
	return missing, nil
}

// retainSetup stores the package and config file bytes snap names, returning the package
// references. It records each package's freshly hashed files in snap.
func (s *SetupJobs) retainSetup(
	ctx context.Context, inst *store.Instance, snap *SetupSnapshot, staging string,
) ([]store.SetupArtifactRef, error) {
	refs, err := (&setupArtifacts{DataRoot: s.DataRoot, Archives: s.Archives}).Save(ctx, inst, snap, staging)
	if err != nil {
		return nil, err
	}
	if err := retainSetupConfigs(ctx, inst, setupblob.New(s.DataRoot), snap.ConfigFiles); err != nil {
		return nil, err
	}
	return refs, nil
}

// captureBeforeRestore saves current, the server's setup before a restore of the setup named
// name, as an automatic setup for the restore's finish to publish.
func (s *SetupJobs) captureBeforeRestore(
	ctx context.Context, inst *store.Instance, name string, current *SetupSnapshot, staging string,
) (*store.SavedSetup, []store.SetupArtifactRef, error) {
	dir := filepath.Join(staging, "before")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create staging for the setup before restore: %w", err)
	}
	snap := *current
	snap.Mods = slices.Clone(current.Mods)
	refs, err := s.retainSetup(ctx, inst, &snap, dir)
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the setup before restore: %w", err)
	}
	return &store.SavedSetup{
		ID: store.NewID(), InstanceID: inst.ID, Name: automaticSetupName(name),
		GameBuildID: deref(inst.GameBuildID), WorldName: inst.WorldName, SnapshotJSON: string(raw),
		Automatic: true, Configs: snap.ConfigFiles, ConfigsTruncated: snap.ConfigsTruncated,
	}, refs, nil
}

// automaticSetupName names the setup saved before restoring the setup named name, cut to
// MaxSetupNameBytes without splitting a character.
func automaticSetupName(name string) string {
	full := "Before restoring " + name
	for len(full) > MaxSetupNameBytes {
		_, size := utf8.DecodeLastRuneInString(full)
		full = full[:len(full)-size]
	}
	return full
}
