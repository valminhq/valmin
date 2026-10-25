package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

const (
	// maxTrialNameAttempts bounds how many names a copy tries before giving up.
	maxTrialNameAttempts = 20
	// applyStarted is the checkpoint of an update-apply job from which it can no longer be
	// cancelled.
	applyStarted = "apply_started"
)

// UpdateTestExistsError reports that a server already has an update-test copy.
type UpdateTestExistsError struct{ CopyID string }

func (e *UpdateTestExistsError) Error() string {
	return fmt.Sprintf("an update test copy %s already exists", e.CopyID)
}

// LowMemoryError reports that the host has less memory available than the copy would be
// allowed to use.
type LowMemoryError struct{ AvailableMB, NeededMB int }

func (e *LowMemoryError) Error() string {
	return fmt.Sprintf("the host has %d MB of memory available and the copy may use %d MB",
		e.AvailableMB, e.NeededMB)
}

// ErrNestedUpdateTest is a refusal to test an update on an update-test copy.
var ErrNestedUpdateTest = errors.New("an update test copy cannot itself be tested")

// UpdateTester runs the jobs that try an update on a copy of a server and then apply it to the
// server or leave it to be discarded.
type UpdateTester struct {
	DB          *store.DB
	Engine      *jobs.Engine
	Chainer     *Chainer
	Cloner      *Cloner
	GameUpdater *GameUpdater
	Deleter     *Deleter
	// Installer is the mod engine. Nil leaves mod updates untestable.
	Installer *manager.Installer
	Ports     *instance.Allocator
	Streams   *instance.Streams
	ReadMods  func(context.Context, *store.Instance) ([]store.InstanceMod, error)
	DataRoot  string
	// CPUs is the host's core count, the base of a copy's CPU limit when its server has none.
	CPUs int
	// AvailableMemoryMB reads the host's available memory.
	AvailableMemoryMB func() (int, error)
}

// UpdateTestPayload is what an update-test job records.
type UpdateTestPayload struct {
	Game   bool   `json:"game"`
	Mods   bool   `json:"mods"`
	CopyID string `json:"copy_id"`
}

// UpdateTestSubmission carries the inputs recorded when an update test is queued.
type UpdateTestSubmission struct {
	Instance    *store.Instance
	Game, Mods  bool
	RequestedBy string
	Audit       *store.AuditEntry
}

// Submit queues an update test of sub.Instance. It refuses a server that has a copy already, a
// copy itself, and a host without the memory the copy may use.
func (t *UpdateTester) Submit(ctx context.Context, sub *UpdateTestSubmission) (*store.Job, error) {
	src := sub.Instance
	if src.TestOf != nil {
		return nil, ErrNestedUpdateTest
	}
	existing, err := t.DB.InstanceTestCopy(ctx, src.ID)
	if err != nil {
		return nil, fmt.Errorf("check for an existing update test copy: %w", err)
	}
	if existing != nil {
		return nil, &UpdateTestExistsError{CopyID: existing.ID}
	}
	if err := t.checkMemory(ctx, src); err != nil {
		return nil, err
	}

	payload := UpdateTestPayload{Game: sub.Game, Mods: sub.Mods, CopyID: store.NewID()}
	id := src.ID
	job, err := t.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindUpdateTest, LockKey: jobs.ChainLockKey(id),
		InstanceID: &id, InstanceName: src.Name, RequestedBy: sub.RequestedBy,
		Payload: payload, Audit: sub.Audit,
	}, t.runTest(src, &payload, sub.RequestedBy))
	if err != nil {
		return nil, fmt.Errorf("submit update test for instance %s: %w", id, err)
	}
	return job, nil
}

// checkMemory refuses a copy the host has no memory for. A host whose memory cannot be read is
// let through with a warning.
func (t *UpdateTester) checkMemory(ctx context.Context, src *store.Instance) error {
	available, err := t.AvailableMemoryMB()
	if err != nil {
		slog.WarnContext(ctx, "could not read the host's available memory for an update test",
			slog.String("instance_id", src.ID), slog.Any("error", err))
		return nil
	}
	if available < src.MemLimitMB {
		return &LowMemoryError{AvailableMB: available, NeededMB: src.MemLimitMB}
	}
	return nil
}

// testCPULimit is half the server's CPU limit, or half the host's cores when it has none, and
// never below one core.
func (t *UpdateTester) testCPULimit(src *store.Instance) *float64 {
	cores := float64(t.CPUs)
	if src.CPULimit != nil {
		cores = *src.CPULimit
	}
	limit := max(1, cores/2)
	return &limit
}

// runTest is the update-test job: copy the server, put the updates on the copy, boot it and
// report which mods failed to load. The copy is left running.
func (t *UpdateTester) runTest(src *store.Instance, p *UpdateTestPayload, requestedBy string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 5, "copying the server")
		copyInst, err := t.cloneForTest(ctx, jh, src, p.CopyID, requestedBy)
		if err != nil {
			return trialFailure(ctx, err, "copy the server")
		}
		jh.Log(fmt.Sprintf("the copy is %q, limited to %s", copyInst.Name, limitsText(copyInst)))

		if p.Game {
			jh.Progress(ctx, 30, "installing the new game build on the copy")
			err := t.updateGame(ctx, jh, copyInst.ID, "", requestedBy)
			if err != nil {
				return trialFailure(ctx, err, "update the game on the copy")
			}
		}
		if p.Mods {
			jh.Progress(ctx, 55, "updating mods on the copy")
			if err := t.updateMods(ctx, jh, copyInst.ID, nil, requestedBy); err != nil {
				return trialFailure(ctx, err, "update the mods on the copy")
			}
		}

		jh.Progress(ctx, 70, "starting the copy")
		if err := t.Chainer.runStep(
			ctx,
			jh,
			trialChain(copyInst, requestedBy),
			&ChainStep{Type: StepStart},
		); err != nil {
			return trialFailure(ctx, err, "start the copy")
		}
		jh.Progress(ctx, 100, t.bootReport(ctx, jh, copyInst.ID))
		return jobs.Outcome{
			Status: jobs.StatusSucceeded,
			OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxMarkTestPassed(ctx, tx, copyInst.ID)
			},
		}
	}
}

// limitsText describes a copy's memory and CPU limits for the job log.
func limitsText(inst *store.Instance) string {
	cpu := "all cores"
	if inst.CPULimit != nil {
		cpu = fmt.Sprintf("%.1f cores", *inst.CPULimit)
	}
	return fmt.Sprintf("%d MB of memory and %s", inst.MemLimitMB, cpu)
}

// trialChain is the submission runStep needs to run a step against inst.
func trialChain(inst *store.Instance, requestedBy string) *ChainSubmission {
	return &ChainSubmission{Instance: inst, RequestedBy: requestedBy}
}

// trialFailure maps the error of one step to the job's outcome.
func trialFailure(ctx context.Context, err error, what string) jobs.Outcome {
	switch {
	case errors.Is(err, jobs.ErrCancelRequested):
		return jobs.Outcome{Status: jobs.StatusCancelled}
	case ctx.Err() != nil, errors.Is(err, jobs.ErrShuttingDown):
		return jobs.Failed(errcode.Interrupted, fmt.Errorf("%s: %w", what, err))
	default:
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: chainFailureCode(err),
			Error: fmt.Sprintf("%s: %v", what, err),
		}
	}
}

// cloneForTest submits the copy's clone job, waits for it and returns the copy's row.
func (t *UpdateTester) cloneForTest(
	ctx context.Context, jh *jobs.Handle, src *store.Instance, copyID, requestedBy string,
) (*store.Instance, error) {
	name := src.Name + " update test"
	archiveID := store.NewID()
	run := &CloneRun{
		Source: src,
		Destination: &store.Instance{
			ID: copyID, Name: name, DataDir: instance.DataDir(t.DataRoot, copyID),
			CPULimit: t.testCPULimit(src),
		},
		ArchiveID: archiveID, RequestedBy: requestedBy, UpdateTest: true,
		ArchivePath: filepath.Join(instance.BackupsDir(t.DataRoot), copyID,
			backup.Name(name, "clone", archiveID)),
	}
	fresh, err := t.Chainer.fresh(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	run.Source = fresh
	if run.liveSource() {
		jh.Log("the server is running, so its world is copied as of the game's last save")
	}
	job, err := t.submitClone(ctx, run)
	if err != nil {
		return nil, err
	}
	if err := t.Chainer.waitChild(ctx, jh, job); err != nil {
		return nil, err
	}
	return t.Chainer.fresh(ctx, copyID)
}

// submitClone queues the clone under the first free name: the one in run, then that name with
// 2, 3 and so on appended, since a server may already hold the first.
func (t *UpdateTester) submitClone(ctx context.Context, run *CloneRun) (*store.Job, error) {
	base := run.Destination.Name
	for n := 1; ; n++ {
		if n > 1 {
			run.Destination.Name = fmt.Sprintf("%s %d", base, n)
		}
		job, err := t.Cloner.SubmitOnFreePort(ctx, t.Ports, run)
		if !errors.Is(err, store.ErrInstanceNameTaken) || n == maxTrialNameAttempts {
			return job, err
		}
	}
}

// updateGame installs a game build on the stopped server id and waits for it. An empty buildID
// installs the branch's current build.
func (t *UpdateTester) updateGame(ctx context.Context, jh *jobs.Handle, id, buildID, requestedBy string) error {
	inst, err := t.Chainer.fresh(ctx, id)
	if err != nil {
		return err
	}
	job, err := t.GameUpdater.Submit(ctx, &GameUpdateSubmission{
		Instance: inst, Confirmed: true, RequestedBy: requestedBy, BuildID: buildID,
	})
	if err != nil {
		return err
	}
	return t.Chainer.waitChild(ctx, jh, job)
}

// updateMods updates mods on the stopped server id and waits for it. Nil targets mean every
// pending update. No targets to apply is not an error.
func (t *UpdateTester) updateMods(
	ctx context.Context, jh *jobs.Handle, id string, targets []manager.UpdateTarget, requestedBy string,
) error {
	if t.Installer == nil {
		return errors.New("this panel has no mod engine")
	}
	if targets == nil {
		var err error
		if targets, err = t.Installer.Planner().PendingUpdates(ctx, id); err != nil {
			return fmt.Errorf("find the pending mod updates: %w", err)
		}
	}
	if len(targets) == 0 {
		jh.Log("no mod updates are pending")
		return nil
	}
	inst, err := t.Chainer.fresh(ctx, id)
	if err != nil {
		return err
	}
	job, err := t.Installer.Submit(ctx, &manager.InstallSubmission{
		Instance: inst, RequestedBy: requestedBy,
		Payload: &manager.InstallPayload{Updates: targets, Backup: true, Unowned: installer.UnownedKeep},
	})
	if err != nil {
		return err //nolint:wrapcheck // the caller reads the job conflict
	}
	return t.Chainer.waitChild(ctx, jh, job)
}

// bootReport logs what the copy's loader said about each plugin and returns the job's result
// line. A copy whose loader log says nothing is reported as such rather than as a success.
func (t *UpdateTester) bootReport(ctx context.Context, jh *jobs.Handle, copyID string) string {
	copyInst, err := t.Chainer.fresh(ctx, copyID)
	if err != nil {
		return "the copy started; its mods could not be checked: " + err.Error()
	}
	load, err := instance.ReadPluginLoad(copyInst.DataDir, t.Streams.Patterns())
	switch {
	case err != nil:
		return "the copy started; its mods could not be checked: " + err.Error()
	case load == nil:
		return "the copy started; the mod loader wrote no log, so no mod was checked"
	}
	failed := make([]string, 0, len(load.Failed))
	for _, f := range load.Failed {
		jh.Log(fmt.Sprintf("%s %s did not load: %s", f.Name, f.Version, f.Reason))
		failed = append(failed, f.Name)
	}
	if len(failed) == 0 {
		return fmt.Sprintf("the copy started; %d mods loaded and none failed", load.LoadedCount())
	}
	return fmt.Sprintf("the copy started; %d mods loaded and %d failed: %s",
		load.LoadedCount(), len(failed), strings.Join(failed, ", "))
}

// TestChanges is what an update-test copy runs that its source does not.
type TestChanges struct {
	// SourceBuildID and CopyBuildID are the game builds the two servers have installed.
	SourceBuildID, CopyBuildID string
	// Mods are the packages the copy runs at a newer version than its source, at the copy's
	// version.
	Mods []manager.UpdateTarget
}

// GameChanged reports whether the copy runs another game build than its source.
func (c *TestChanges) GameChanged() bool { return c.CopyBuildID != c.SourceBuildID }

// GameNewer reports whether the copy runs a newer game build than its source. Build ids are
// numbers that grow; ids that are not numbers count as newer when they differ.
func (c *TestChanges) GameNewer() bool {
	copyID, copyErr := strconv.ParseUint(c.CopyBuildID, 10, 64)
	sourceID, sourceErr := strconv.ParseUint(c.SourceBuildID, 10, 64)
	if copyErr != nil || sourceErr != nil {
		return c.GameChanged()
	}
	return copyID > sourceID
}

// Changes compares the game build and the installed mod versions of src and its test copy.
func (t *UpdateTester) Changes(ctx context.Context, src, testCopy *store.Instance) (TestChanges, error) {
	var c TestChanges
	var err error
	if c.SourceBuildID, err = instance.InstalledBuildID(src.DataDir); err != nil {
		return c, fmt.Errorf("read the game build of %s: %w", src.Name, err)
	}
	if c.CopyBuildID, err = instance.InstalledBuildID(testCopy.DataDir); err != nil {
		return c, fmt.Errorf("read the game build of %s: %w", testCopy.Name, err)
	}
	srcMods, err := t.ReadMods(ctx, src)
	if err != nil {
		return c, fmt.Errorf("read the mods of %s: %w", src.Name, err)
	}
	copyMods, err := t.ReadMods(ctx, testCopy)
	if err != nil {
		return c, fmt.Errorf("read the mods of %s: %w", testCopy.Name, err)
	}
	c.Mods = diffMods(srcMods, copyMods)
	return c, nil
}

// diffMods lists the packages installed in both sets that are newer in the second, at the
// version of the second. A package only one set has is not an update of the other, and a
// package that is older in the second, because the first moved on, is not applied.
func diffMods(from, to []store.InstanceMod) []manager.UpdateTarget {
	installed := make(map[string]string, len(from))
	for i := range from {
		installed[from[i].FullName] = from[i].Version
	}
	var targets []manager.UpdateTarget
	for i := range to {
		m := &to[i]
		if was, ok := installed[m.FullName]; ok && manager.Newer(m.Version, was) {
			targets = append(targets, manager.UpdateTarget{
				FullName: m.FullName, Source: m.Source.String(), FromVersion: was, Version: m.Version,
			})
		}
	}
	return targets
}

// UpdateApplyPayload is what an update-apply job records.
type UpdateApplyPayload struct {
	CopyID string `json:"copy_id"`
	// BuildID is the game build to install, empty when the server already runs it.
	BuildID string                 `json:"build_id,omitempty"`
	Mods    []manager.UpdateTarget `json:"mods,omitempty"`
}

// UpdateApplySubmission carries the inputs recorded when an apply is queued.
type UpdateApplySubmission struct {
	Instance    *store.Instance
	RequestedBy string
	Audit       *store.AuditEntry
}

// ErrNoUpdateTest is a refusal to apply a test that has no copy.
var ErrNoUpdateTest = errors.New("this server has no update test copy")

// ErrUpdateTestUnsettled is a refusal to apply a copy that is still being created or changed,
// or that failed and was parked.
var ErrUpdateTestUnsettled = errors.New("the update test copy is not ready to be applied")

// ApplyBlocker says why testCopy's updates cannot be applied to src, or returns nil. A copy
// can be applied once it is stopped or running and its test ran to the end: a test that failed
// or was cancelled may have left it with only part of the update, and never booted.
func (t *UpdateTester) ApplyBlocker(ctx context.Context, src, testCopy *store.Instance) error {
	if state := instance.State(testCopy.State); state != instance.StateStopped && state != instance.StateRunning {
		return fmt.Errorf("%w: it is %s", ErrUpdateTestUnsettled, state)
	}
	if !testCopy.TestPassed {
		return fmt.Errorf("%w: its test has not run to the end", ErrUpdateTestUnsettled)
	}
	return nil
}

// SubmitApply queues the installation of what sub.Instance's test copy runs onto the server.
func (t *UpdateTester) SubmitApply(ctx context.Context, sub *UpdateApplySubmission) (*store.Job, error) {
	src := sub.Instance
	testCopy, err := t.DB.InstanceTestCopy(ctx, src.ID)
	if err != nil {
		return nil, fmt.Errorf("find the update test copy: %w", err)
	}
	if testCopy == nil {
		return nil, ErrNoUpdateTest
	}
	if err := t.ApplyBlocker(ctx, src, testCopy); err != nil {
		return nil, err
	}
	changes, err := t.Changes(ctx, src, testCopy)
	if err != nil {
		return nil, err
	}
	payload := UpdateApplyPayload{CopyID: testCopy.ID, Mods: changes.Mods}
	if changes.GameNewer() {
		payload.BuildID = changes.CopyBuildID
	}
	id := src.ID
	job, err := t.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindUpdateApply, LockKey: jobs.ChainLockKey(id),
		InstanceID: &id, InstanceName: src.Name, RequestedBy: sub.RequestedBy,
		Payload: payload, Audit: sub.Audit,
	}, t.runApply(src.ID, &payload, sub.RequestedBy))
	if err != nil {
		return nil, fmt.Errorf("submit update apply for instance %s: %w", id, err)
	}
	return job, nil
}

// runApply is the update-apply job: stop the server if it runs, install the tested game build
// and mod versions, start it again if it was running and delete the copy. A failure leaves the
// copy in place. The job can be cancelled until it records applyStarted.
func (t *UpdateTester) runApply(srcID string, p *UpdateApplyPayload, requestedBy string) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		src, err := t.beginApply(ctx, jh, srcID)
		if err != nil {
			return trialFailure(ctx, err, "begin the apply")
		}
		wasRunning := instance.State(src.State) == instance.StateRunning
		// A server is only stopped when there is something to install on it.
		stops := wasRunning && (p.BuildID != "" || len(p.Mods) > 0)
		for i, step := range t.applySteps(src, p, requestedBy, stops) {
			jh.Progress(ctx, step.progress, step.message)
			if err := step.run(ctx, jh); err != nil {
				// The stop is the first step, so a later one failing means it succeeded.
				if stops && i > 0 {
					jh.Log("the server was stopped for the update and has not been started again")
				}
				return trialFailure(ctx, err, step.failure)
			}
		}

		jh.Progress(ctx, 90, "deleting the copy")
		if err := t.deleteCopy(ctx, jh, p.CopyID, requestedBy); err != nil {
			if ctx.Err() != nil {
				return trialFailure(ctx, err, "delete the copy")
			}
			jh.Log("the update is applied, but the copy was not deleted: " + err.Error())
		}
		jh.Progress(ctx, 100, applyReport(p, wasRunning))
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	}
}

// beginApply reads the server and closes cancellation. A cancel requested before that is
// returned as jobs.ErrCancelRequested, with nothing changed.
func (t *UpdateTester) beginApply(ctx context.Context, jh *jobs.Handle, srcID string) (*store.Instance, error) {
	src, err := t.Chainer.fresh(ctx, srcID)
	if err != nil {
		return nil, err
	}
	if jh.CancelRequested(ctx) {
		return nil, jobs.ErrCancelRequested
	}
	if err := jh.Checkpoint(ctx, applyStarted); err != nil {
		return nil, fmt.Errorf("record the start: %w", err)
	}
	return src, nil
}

// trialStep is one step of an apply: the progress and message shown while it runs, how its
// failure is described and what it does.
type trialStep struct {
	progress         int
	message, failure string
	run              func(context.Context, *jobs.Handle) error
}

// applySteps is the work of an apply on src, in order: stop it if it runs, install the tested
// game build and mod versions where they differ, and start it again if it was running.
func (t *UpdateTester) applySteps(
	src *store.Instance, p *UpdateApplyPayload, requestedBy string, stops bool,
) []trialStep {
	chain := trialChain(src, requestedBy)
	runChainStep := func(typ string) func(context.Context, *jobs.Handle) error {
		return func(ctx context.Context, jh *jobs.Handle) error {
			return t.Chainer.runStep(ctx, jh, chain, &ChainStep{Type: typ})
		}
	}
	var steps []trialStep
	if stops {
		steps = append(steps, trialStep{5, "stopping the server", "stop the server", runChainStep(StepStop)})
	}
	if p.BuildID != "" {
		steps = append(steps, trialStep{
			20, "installing game build " + p.BuildID, "update the game",
			func(ctx context.Context, jh *jobs.Handle) error {
				return t.updateGame(ctx, jh, src.ID, p.BuildID, requestedBy)
			},
		})
	}
	if len(p.Mods) > 0 {
		steps = append(steps, trialStep{
			50, "updating mods", "update the mods",
			func(ctx context.Context, jh *jobs.Handle) error {
				return t.updateMods(ctx, jh, src.ID, p.Mods, requestedBy)
			},
		})
	}
	if stops {
		steps = append(steps, trialStep{75, "starting the server", "start the server", runChainStep(StepStart)})
	}
	return steps
}

// deleteCopy stops the copy if it runs and deletes it with its world and backups.
func (t *UpdateTester) deleteCopy(ctx context.Context, jh *jobs.Handle, copyID, requestedBy string) error {
	testCopy, err := t.Chainer.fresh(ctx, copyID)
	if err != nil {
		return err
	}
	if instance.State(testCopy.State) == instance.StateRunning {
		if err := t.Chainer.runStep(
			ctx,
			jh,
			trialChain(testCopy, requestedBy),
			&ChainStep{Type: StepStop},
		); err != nil {
			return fmt.Errorf("stop the copy: %w", err)
		}
		if testCopy, err = t.Chainer.fresh(ctx, copyID); err != nil {
			return err
		}
	}
	job, err := t.Deleter.Submit(ctx, &DeleteSubmission{Instance: testCopy, RequestedBy: requestedBy})
	if err != nil {
		return err
	}
	return t.Chainer.waitChild(ctx, jh, job)
}

// applyReport is the job's result line; running says whether the server runs afterwards.
func applyReport(p *UpdateApplyPayload, running bool) string {
	var parts []string
	if p.BuildID != "" {
		parts = append(parts, "game build "+p.BuildID)
	}
	if n := len(p.Mods); n > 0 {
		parts = append(parts, fmt.Sprintf("%d mod updates", n))
	}
	what := "no changes were needed"
	if len(parts) > 0 {
		what = "applied " + strings.Join(parts, " and ")
	}
	state := "the server is stopped"
	if running {
		state = "the server is running"
	}
	return what + "; " + state
}
