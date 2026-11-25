package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
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
// name, linked to that archive and recorded as caused by requestedBy. It returns the finish step
// that records both, nil when there is nothing to record. The archive is required; a setup that
// cannot be captured is logged on jh and left out. scratch is a directory the capture may use
// and the caller removes.
func (s *SetupJobs) Checkpoint(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, name, requestedBy, scratch string,
) (func(context.Context, *sql.Tx) error, error) {
	world, err := s.Worlds.snapshotRow(ctx, inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, err
	}
	var record, link func(context.Context, *sql.Tx) error
	setup := automaticSetup{name: name, createdBy: requestedBy, staging: scratch, before: "update"}
	if world == nil {
		jh.Log("no world archive was taken: this server has no world yet")
	} else {
		record, setup.worldID = recordSnapshot(world), world.ID
		link = s.linkPending(ctx, jh, inst, world.ID)
		setup.linked = link != nil
	}
	if inst.SetupBeforeUpdate != string(SetupBeforeUpdateOff) {
		jh.Progress(ctx, 66, "saving the current setup")
	}
	return joinFinish(record, link, s.saveVersion(ctx, jh, inst, &setup)), nil
}

// linkPending returns the finish step that links the world backup worldID, being taken before
// inst's mods change, to each of inst's setups waiting for one, naming them on jh; nil when none
// waits.
func (s *SetupJobs) linkPending(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, worldID string,
) func(context.Context, *sql.Tx) error {
	pending, err := s.DB.PendingSetups(ctx, inst.ID)
	if err != nil {
		jh.Log(fmt.Sprintf("warning: setups waiting for a world backup were not linked: %v", err))
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(pending))
	for i := range pending {
		ids = append(ids, pending[i].ID)
		jh.Log(fmt.Sprintf("setup %q is linked to this world backup", pending[i].Name))
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		return store.TxLinkPendingSetups(ctx, tx, inst.ID, worldID, ids)
	}
}

// SaveVersion saves inst's current setup as an automatic setup named name, recorded as caused by
// requestedBy, before a mod change that archives no world, unless inst turns it off. It returns
// the finish step that records it, nil when nothing was saved; a setup that cannot be captured
// is logged on jh. scratch is a directory the capture may use and the caller removes.
func (s *SetupJobs) SaveVersion(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, name, requestedBy, scratch string,
) func(context.Context, *sql.Tx) error {
	setup := automaticSetup{name: name, createdBy: requestedBy, staging: scratch, before: "change"}
	return s.saveVersion(ctx, jh, inst, &setup)
}

// saveVersion captures inst's current setup as the automatic setup a describes, light or full as
// inst asks, and returns the finish step that saves it; nil when inst turns it off or the capture
// fails, which is logged on jh.
func (s *SetupJobs) saveVersion(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, a *automaticSetup,
) func(context.Context, *sql.Tx) error {
	mode, _ := ParseSetupBeforeUpdate(inst.SetupBeforeUpdate)
	if mode == SetupBeforeUpdateOff {
		return nil
	}
	snap, err := (&SetupState{DB: s.DB}).Capture(ctx, inst)
	if err != nil {
		a.warn(ctx, jh, inst, err)
		return nil
	}
	snap.Light = mode == SetupBeforeUpdateLight
	return s.saveAutomatic(ctx, jh, inst, &snap, a)
}

// automaticSetup is a setup the panel saves on its own before it changes a server.
type automaticSetup struct {
	name string
	// createdBy is the user whose action the setup comes before, "" for the panel's own.
	createdBy string
	// worldID is the world backup the setup links, "" for none.
	worldID string
	// linked reports that the same finish links worldID to the setups waiting for one.
	linked bool
	// spare is an automatic setup pruning keeps whatever its age: the one being restored.
	spare string
	// staging is a scratch directory for the capture.
	staging string
	// before names what is about to happen, for the job log: "restore", "update" or "change".
	before string
}

// warn records on jh and in the log that the automatic setup was not saved.
func (a *automaticSetup) warn(ctx context.Context, jh *jobs.Handle, inst *store.Instance, err error) {
	jh.Log(fmt.Sprintf("warning: the setup before this %s was not saved: %v", a.before, err))
	slog.WarnContext(ctx, "automatic setup not saved", slog.String("instance_id", inst.ID),
		slog.String("before", a.before), slog.Any("error", err))
}

// saveAutomatic retains current's package and config files as the automatic setup a describes,
// and returns the finish step that publishes it and applies inst's setup retention, sparing
// a.spare. It returns nil when the capture fails, which is logged on jh, or when inst's newest
// setup already holds the same setup and world.
func (s *SetupJobs) saveAutomatic(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, current *SetupSnapshot, a *automaticSetup,
) func(context.Context, *sql.Tx) error {
	row, refs, err := s.captureAutomatic(ctx, inst, current, a)
	if err != nil {
		a.warn(ctx, jh, inst, err)
		return nil
	}
	if same := s.coveredByNewest(ctx, row, a.linked); same != nil {
		jh.Log(fmt.Sprintf("the setup before this %s is unchanged since setup %q; no new one was saved",
			a.before, same.Name))
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxSaveSetup(ctx, tx, row, refs); err != nil {
			return err
		}
		keep := store.SetupRetention{Versions: inst.SetupVersionsKept, Worlds: inst.SetupWorldsKept}
		return store.TxPruneAutomaticSetups(ctx, tx, inst.ID, keep, a.spare)
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
		ID: store.NewID(), InstanceID: inst.ID, Name: setupName(a.name), CreatedBy: a.createdBy,
		GameBuildID: deref(inst.GameBuildID), WorldName: inst.WorldName, SnapshotJSON: string(raw),
		BackupID: a.worldID, Automatic: true, Configs: snap.ConfigFiles,
		ConfigsTruncated: snap.ConfigsTruncated, ModsFingerprint: ModsFingerprint(snap.Mods),
	}, refs, nil
}

// coveredByNewest returns the instance's newest setup when it holds what row holds and row links
// no world backup, or linked reports the same finish links row's backup to the newest setup,
// which waits for one; nil otherwise or when it cannot be read.
func (s *SetupJobs) coveredByNewest(ctx context.Context, row *store.SavedSetup, linked bool) *store.SavedSetup {
	newest, _, err := s.DB.NewestSetup(ctx, row.InstanceID)
	if err != nil || newest == nil {
		return nil
	}
	if row.BackupID != "" && (!linked || !newest.WorldPending) {
		return nil
	}
	if !sameSetup(row, newest) {
		return nil
	}
	return newest
}

// sameSetup reports whether b holds what a holds: the same game build, launch settings, mods with
// their files, and config files, and every package a keeps unless b is light only when a is. A
// setup saved before setups covered the config tree is never the same.
func sameSetup(a, b *store.SavedSetup) bool {
	if a.GameBuildID != b.GameBuildID {
		return false
	}
	snapA, errA := DecodeSetupSnapshot(a)
	snapB, errB := DecodeSetupSnapshot(b)
	if errA != nil || errB != nil || !snapA.ConfigTree || !snapB.ConfigTree || (snapB.Light && !snapA.Light) {
		return false
	}
	launchA, errA := json.Marshal(snapA.Instance)
	launchB, errB := json.Marshal(snapB.Instance)
	if errA != nil || errB != nil || !bytes.Equal(launchA, launchB) {
		return false
	}
	byName := func(x, y SetupMod) int { return strings.Compare(x.FullName, y.FullName) }
	modsA, modsB := slices.Clone(snapA.Mods), slices.Clone(snapB.Mods)
	slices.SortFunc(modsA, byName)
	slices.SortFunc(modsB, byName)
	return slices.Equal(modsA, modsB) && slices.Equal(snapA.ConfigFiles, snapB.ConfigFiles)
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
