package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// SetupBeforeUpdate is what a mod update saves of a server's setup before it changes anything.
type SetupBeforeUpdate string

const (
	// SetupBeforeUpdateFull saves a setup that keeps every package file.
	SetupBeforeUpdateFull SetupBeforeUpdate = "full"
	// SetupBeforeUpdateLight saves a light setup, which downloads reproducible packages on restore.
	SetupBeforeUpdateLight SetupBeforeUpdate = "light"
	// SetupBeforeUpdateOff saves no setup; the update still backs up the world.
	SetupBeforeUpdateOff SetupBeforeUpdate = "off"
)

// ParseSetupBeforeUpdate resolves a stored or requested value to its constant.
func ParseSetupBeforeUpdate(s string) (SetupBeforeUpdate, bool) {
	switch v := SetupBeforeUpdate(s); v {
	case SetupBeforeUpdateFull, SetupBeforeUpdateLight, SetupBeforeUpdateOff:
		return v, true
	}
	return "", false
}

// Checkpoint is what a mod update saves before it moves any file: a pre-update archive of
// inst's world and, unless inst turns it off, its current setup as an automatic setup named
// name and linked to that archive. It returns the finish step that records both, nil when there
// is nothing to record. The archive is required; a setup that cannot be captured is logged on
// jh and left out. scratch is a directory the capture may use and the caller removes.
func (s *SetupJobs) Checkpoint(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, name, scratch string,
) (func(context.Context, *sql.Tx) error, error) {
	world, err := s.Worlds.snapshotRow(ctx, inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, err
	}
	var record func(context.Context, *sql.Tx) error
	var worldID string
	if world == nil {
		jh.Log("no world archive was taken: this server has no world yet")
	} else {
		record, worldID = recordSnapshot(world), world.ID
	}
	mode, _ := ParseSetupBeforeUpdate(inst.SetupBeforeUpdate)
	if mode == SetupBeforeUpdateOff {
		return record, nil
	}
	jh.Progress(ctx, 66, "saving the current setup")
	setup := automaticSetup{name: name, worldID: worldID, staging: scratch, before: "update"}
	snap, err := (&SetupState{DB: s.DB}).Capture(ctx, inst)
	if err != nil {
		setup.warn(ctx, jh, inst, err)
		return record, nil
	}
	snap.Light = mode == SetupBeforeUpdateLight
	return joinFinish(record, s.saveAutomatic(ctx, jh, inst, &snap, &setup)), nil
}

// automaticSetup is a setup the panel saves on its own before it changes a server.
type automaticSetup struct {
	name string
	// worldID is the world backup the setup links, "" for none.
	worldID string
	// spare is an automatic setup pruning keeps whatever its age: the one being restored.
	spare string
	// staging is a scratch directory for the capture.
	staging string
	// before names what is about to happen, for the job log: "restore" or "update".
	before string
}

// warn records on jh and in the log that the automatic setup was not saved.
func (a *automaticSetup) warn(ctx context.Context, jh *jobs.Handle, inst *store.Instance, err error) {
	jh.Log(fmt.Sprintf("warning: the setup before this %s was not saved: %v", a.before, err))
	slog.WarnContext(ctx, "automatic setup not saved", slog.String("instance_id", inst.ID),
		slog.String("before", a.before), slog.Any("error", err))
}

// saveAutomatic retains current's package and config files as the automatic setup a describes,
// and returns the finish step that publishes it and prunes older automatic setups other than
// a.spare. A capture that fails is logged on jh and returns nil.
func (s *SetupJobs) saveAutomatic(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, current *SetupSnapshot, a *automaticSetup,
) func(context.Context, *sql.Tx) error {
	row, refs, err := s.captureAutomatic(ctx, inst, current, a)
	if err != nil {
		a.warn(ctx, jh, inst, err)
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxSaveSetup(ctx, tx, row, refs); err != nil {
			return err
		}
		return store.TxPruneAutomaticSetups(ctx, tx, inst.ID, keepAutomaticSetups, a.spare)
	}
}

// captureAutomatic stores current's package and config bytes and builds the automatic setup row
// a describes.
func (s *SetupJobs) captureAutomatic(
	ctx context.Context, inst *store.Instance, current *SetupSnapshot, a *automaticSetup,
) (*store.SavedSetup, []store.SetupArtifactRef, error) {
	if err := os.MkdirAll(a.staging, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create staging for the automatic setup: %w", err)
	}
	snap := *current
	snap.Mods = slices.Clone(current.Mods)
	refs, err := s.retainSetup(ctx, inst, &snap, a.staging)
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the automatic setup: %w", err)
	}
	return &store.SavedSetup{
		ID: store.NewID(), InstanceID: inst.ID, Name: setupName(a.name),
		GameBuildID: deref(inst.GameBuildID), WorldName: inst.WorldName, SnapshotJSON: string(raw),
		BackupID: a.worldID, Automatic: true, Configs: snap.ConfigFiles,
		ConfigsTruncated: snap.ConfigsTruncated,
	}, refs, nil
}

// setupName is name cut to MaxSetupNameBytes without splitting a character.
func setupName(name string) string {
	for len(name) > MaxSetupNameBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// joinFinish runs the finish steps that are not nil in order, and is nil when all are.
func joinFinish(steps ...func(context.Context, *sql.Tx) error) func(context.Context, *sql.Tx) error {
	steps = slices.DeleteFunc(steps, func(f func(context.Context, *sql.Tx) error) bool { return f == nil })
	if len(steps) == 0 {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		for _, step := range steps {
			if err := step(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	}
}
