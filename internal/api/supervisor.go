package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/ws"
)

// observeInterval is how often the observer asks Docker what happened. Polling beats an
// event bus for a handful of containers: one labelled List every ten seconds costs less
// than reconnect handling, and is well inside the time anyone takes to notice an outage.
const observeInterval = 10 * time.Second

// Supervisor is the second permitted writer of instances.state — the observer, which
// records what Docker did without being asked — and owns the startup recovery sequence.
//
// It lives in internal/api despite serving no HTTP because recovering a `provisioning` or
// `deleting` row does not merely set a state: it re-submits the job, using the same Runners
// the instance handlers build. One reliability story per operation means the recovery path
// runs identical code rather than a second copy that drifts.
type Supervisor struct {
	inst  *Instances
	crash *instance.CrashLoop
	// hub is the observer's publisher: it writes instances.state too, and a transition
	// nobody announced leaves the dashboard wrong until the next reload.
	hub *ws.Hub
}

// NewSupervisor builds the observer over the same dependencies the instance handlers hold.
func NewSupervisor(inst *Instances) *Supervisor {
	return &Supervisor{inst: inst, crash: instance.NewCrashLoop()}
}

// Recover runs the sweep, then the reconcile, then the resume intents, in that order and
// no other.
//
// The sweep precedes the reconcile (C6): reconciling first would meet instances in a
// transient state whose lock is held by a process that no longer exists, and have to reason
// about whether to touch them. Sweeping first means the reconciler only ever sees unlocked
// instances.
//
// The startup gate and the daemon lease are the caller's. Re-opening the log streams falls
// out of the reconcile pass, which opens a reader for every running container it finds.
func (s *Supervisor) Recover(ctx context.Context) error {
	resume, err := s.sweep(ctx)
	if err != nil {
		return err
	}
	if err := s.reconcile(ctx); err != nil {
		return err
	}
	s.resumeIntents(ctx, resume)
	return nil
}

// Run is the observer loop: the same reconciliation pass, on a timer, for the life of the
// process. It returns when ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(observeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The readers outlive any one request context by design, so shutdown is the one
			// place that has to end them.
			s.inst.Streams.Shutdown()
			return
		case <-ticker.C:
			if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "observer pass failed, will retry", slog.Any("error", err))
			}
		}
	}
}

// sweep closes out every row still marked `running` whose lease_owner is not this boot's:
// those belong to a dead process. It returns the instance ids whose swept job carried a
// resume intent this build is allowed to honour.
//
// No kind is continued in place. Start, stop and restart have no checkpoints, delete is
// idempotent, and a provision resume is a fresh job submitted once the lock is free. So
// every swept row is closed out as `interrupted` with its lock released, and what happens
// next is reconciliation's decision, not the sweep's.
func (s *Supervisor) sweep(ctx context.Context) (resume []string, err error) {
	stale, err := s.inst.DB.StaleJobs(ctx, s.inst.Engine.Owner())
	if err != nil {
		return nil, fmt.Errorf("sweep dead jobs: %w", err)
	}
	code := apierr.Interrupted.String()
	message := "The panel stopped while this job was running."
	for i := range stale {
		j := &stale[i]
		if err := s.inst.DB.FinishJob(
			ctx, j.ID, "failed", j.Progress, &code, &message, j.Log, j.Clean, time.Now(), nil,
		); err != nil {
			return nil, fmt.Errorf("sweep dead job %s: %w", j.ID, err)
		}
		slog.InfoContext(ctx, "swept dead job",
			slog.String("job_id", j.ID), slog.String("kind", j.Kind),
			slog.Any("instance_id", j.InstanceID), slog.Any("checkpoint", j.Checkpoint))

		s.sweepStaging(ctx, j)

		kind, known := jobs.ByName(j.Kind)
		if j.ResumeAfter && j.InstanceID != nil && known && jobs.ResumeIntentHonoured(kind) {
			resume = append(resume, *j.InstanceID)
		}
	}
	return resume, nil
}

// sweepStaging cleans up after a job the panel was killed in the middle of, per kind. It
// is the only thing left that knows where an interrupted job's staging area was — the path
// is on the job's payload for exactly this.
func (s *Supervisor) sweepStaging(ctx context.Context, j *store.Job) {
	switch j.Kind {
	case jobs.KindWorldImport.String():
		s.sweepImportStaging(ctx, j)
	case jobs.KindModInstall.String():
		s.sweepModInstall(ctx, j)
	case jobs.KindModUninstall.String():
		s.sweepModUninstall(ctx, j)
	}
}

// sweepImportStaging removes the upload directory a killed world import left behind: the
// one case the import job could not clean up itself, because it was not running when the
// panel died.
func (s *Supervisor) sweepImportStaging(ctx context.Context, j *store.Job) {
	var payload worldImportPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted import: payload unreadable, staging left in place",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	if payload.StagingDir == "" {
		return
	}
	root := instance.ImportStagingRoot(s.inst.Cfg.Data.Root)
	if !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(ctx, "interrupted import names a staging directory outside the staging root; not removing",
			slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root))
		return
	}
	if err := os.RemoveAll(payload.StagingDir); err != nil {
		slog.WarnContext(ctx, "interrupted import: staging directory not removed",
			slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir),
			slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "removed the staging directory of an interrupted import",
		slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir))
}

// sweepModInstall rolls an interrupted mod_install back from its manifest rather than
// resuming it. The manifest is written before any file moves, so on a crash it is the exact
// record of what the job was going to place, whether or not it got there; the staging area
// holds the originals of everything it displaced. Together they return server/ to where it
// was.
//
// The packages to undo are read from the staging directory, not from the payload: the
// payload names only what the user asked for, and the resolved closure is what got staged.
// A row exists for a staged package only because this job wrote it, so deleting those rows
// never touches another install's.
func (s *Supervisor) sweepModInstall(ctx context.Context, j *store.Job) {
	var payload modInstallPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted mod install: payload unreadable, nothing rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	root := modStagingRoot(s.inst.Cfg.Data.Root)
	if payload.StagingDir == "" || !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(
			ctx,
			"interrupted mod install names a staging directory outside the staging root; not touching it",
			slog.String("job_id", j.ID),
			slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root),
		)
		return
	}
	defer func() {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "interrupted mod install: staging directory not removed",
				slog.String("job_id", j.ID), slog.Any("error", err))
		}
	}()

	if j.InstanceID == nil {
		return
	}
	inst, err := s.inst.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "interrupted mod install: instance unreadable, nothing rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}

	restore, rolled := s.rollbackStaged(ctx, j, inst, payload.StagingDir)
	if err := s.inst.DB.RollbackInstanceMods(ctx, inst.ID, restore, rolled); err != nil {
		slog.ErrorContext(ctx, "interrupted mod install: rows not restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	if len(rolled)+len(restore) > 0 {
		slog.InfoContext(ctx, "rolled back an interrupted mod install",
			slog.String("job_id", j.ID), slog.Int("packages", len(rolled)+len(restore)))
	}
}

// sweepModUninstall rolls an interrupted mod_uninstall back. The job saves every file it is
// going to remove before removing any of them, and deletes the rows only in its own Finish
// transaction — so an interrupted uninstall still has its rows, and restoring the files
// from the backup is the whole of the recovery.
func (s *Supervisor) sweepModUninstall(ctx context.Context, j *store.Job) {
	var payload modUninstallPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted mod uninstall: payload unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	root := modStagingRoot(s.inst.Cfg.Data.Root)
	if payload.StagingDir == "" || !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(
			ctx,
			"interrupted mod uninstall names a staging directory outside the staging root; not touching it",
			slog.String("job_id", j.ID),
			slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root),
		)
		return
	}
	defer func() {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "interrupted mod uninstall: staging directory not removed",
				slog.String("job_id", j.ID), slog.Any("error", err))
		}
	}()

	if j.InstanceID == nil {
		return
	}
	inst, err := s.inst.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "interrupted mod uninstall: instance unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	installed, err := s.inst.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted mod uninstall: installed mods unreadable, nothing restored",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	byName := make(map[string]string, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = installed[i].FileManifest
	}

	restored := 0
	for _, name := range payload.FullNames {
		manifest, ok := decodeManifest(ctx, j, name, byName)
		if !ok {
			continue
		}
		// The same call the job's own failure path makes: a manifest path with a saved copy
		// goes back, and one without was already gone before the uninstall began.
		if err := installer.Rollback(
			manifest, filepath.Join(inst.DataDir, "server"), stagingBackupDir(payload.StagingDir),
		); err != nil {
			slog.ErrorContext(ctx, "interrupted mod uninstall: files not fully restored",
				slog.String("job_id", j.ID), slog.String("full_name", name), slog.Any("error", err))
			continue
		}
		restored++
	}
	if restored > 0 {
		slog.InfoContext(ctx, "restored the files of an interrupted mod uninstall",
			slog.String("job_id", j.ID), slog.Int("packages", restored))
	}
}

// decodeManifest reads one installed package's file manifest out of the rows read for the
// sweep. A package with no row was never recorded; a row whose manifest will not decode is
// reported and left alone, because guessing at its files is what B9 forbids.
func decodeManifest(
	ctx context.Context, j *store.Job, fullName string, byName map[string]string,
) ([]installer.ManifestEntry, bool) {
	raw, ok := byName[fullName]
	if !ok {
		return nil, false
	}
	var manifest []installer.ManifestEntry
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		slog.ErrorContext(ctx, "interrupted mod job: manifest unreadable, files left as they are",
			slog.String("job_id", j.ID), slog.String("full_name", fullName), slog.Any("error", err))
		return nil, false
	}
	return manifest, true
}

// rollbackStaged undoes every package the interrupted job had staged. It returns the rows
// to put back — the versions an interrupted *update* had already overwritten — and the
// names of the rows to delete. A package with no row is one the job never got as far as
// recording, and nothing of it reached server/ — the rows are written before any file moves.
func (s *Supervisor) rollbackStaged(
	ctx context.Context, j *store.Job, inst *store.Instance, stagingDir string,
) (restore []store.InstanceMod, rolled []string) {
	staged, err := os.ReadDir(filepath.Join(stagingDir, "pkg"))
	if err != nil {
		// Killed before anything was staged, so nothing was written either.
		return nil, nil
	}
	installed, err := s.inst.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted mod install: installed mods unreadable, not rolled back",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return nil, nil
	}
	byName := make(map[string]string, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = installed[i].FileManifest
	}

	serverRoot := filepath.Join(inst.DataDir, "server")
	backupDir := stagingBackupDir(stagingDir)
	for _, d := range staged {
		manifest, ok := decodeManifest(ctx, j, d.Name(), byName)
		if !ok {
			continue
		}
		// What the job had recorded, plus what an update had already removed to make room —
		// read from the staging directory, because from manifest_written onward the row
		// itself describes the new version and no longer names the old version's files.
		prev, err := readPrevRow(stagingDir, d.Name())
		if err != nil {
			slog.ErrorContext(ctx, "interrupted mod install: the replaced row is unreadable, files left in place",
				slog.String("job_id", j.ID), slog.String("full_name", d.Name()), slog.Any("error", err))
			continue
		}
		var stale []string
		if prev != nil {
			stale = prev.Stale
		}
		if err := installer.Rollback(
			rollbackEntries(manifest, stale), serverRoot, backupDir); err != nil {
			slog.ErrorContext(ctx, "interrupted mod install: rollback incomplete",
				slog.String("job_id", j.ID), slog.String("full_name", d.Name()), slog.Any("error", err))
			continue
		}
		if prev != nil {
			restore = append(restore, prev.Row)
			continue
		}
		rolled = append(rolled, d.Name())
	}
	return restore, rolled
}

// withinRoot reports whether path is inside root. The paths it guards come out of a
// database column and feed a recursive delete running as the panel, so a payload naming
// somewhere else entirely gets nothing removed rather than the benefit of the doubt.
func withinRoot(root, path string) bool {
	within, err := filepath.Rel(root, path)
	return err == nil && within != "." && within != ".." &&
		!strings.HasPrefix(within, ".."+string(filepath.Separator))
}

// resumeIntents re-submits the starts a crash interrupted. It runs after reconciliation,
// not before: an instance owes the user a restart only once the matrix has resolved it to a
// state a start can be claimed from.
func (s *Supervisor) resumeIntents(ctx context.Context, instanceIDs []string) {
	for _, id := range instanceIDs {
		inst, err := s.inst.DB.InstanceByID(ctx, id)
		if err != nil || inst == nil {
			slog.WarnContext(ctx, "resume intent: instance unreadable",
				slog.String("instance_id", id), slog.Any("error", err))
			continue
		}
		if instance.State(inst.State) != instance.StateStopped || inst.ContainerID == nil {
			slog.InfoContext(ctx, "resume intent skipped: instance is not startable",
				slog.String("instance_id", id), slog.String("state", inst.State))
			continue
		}
		if _, err := s.inst.submitStart(ctx, inst, *inst.ContainerID, ""); err != nil {
			slog.WarnContext(ctx, "resume intent: start not submitted",
				slog.String("instance_id", id), slog.Any("error", err))
			continue
		}
		slog.InfoContext(ctx, "resumed a server that was running before the panel stopped",
			slog.String("instance_id", id))
	}
}

// reconcile lists the panel's containers, joins them to the DB on the io.valmin.instance.id
// label, and resolves every disagreement. It is also the observer's steady-state pass,
// because those are the same question.
func (s *Supervisor) reconcile(ctx context.Context) error {
	held, err := s.inst.DB.HeldLockKeys(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	instances, err := s.inst.DB.ListInstances(ctx, nil)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	byInstanceID, err := s.managedContainers(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	now := time.Now()
	seen := make(map[string]bool, len(instances))
	for i := range instances {
		inst := &instances[i]
		seen[inst.ID] = true
		// Before the lock check, deliberately: a reader is a stream lifecycle and not a
		// state write, so C14 has nothing to say about it, and a server started by a job
		// should have its console open while that job is still running.
		s.stream(ctx, inst.ID, byInstanceID[inst.ID])
		// C14, and the reason this check is here rather than inside Observe: while a lock is
		// held there is a job making an intentional change, the container will exit because
		// that job stopped it, and an observer that writes on that event races the job that
		// caused it.
		if held[jobs.InstanceLockKey(inst.ID)] {
			continue
		}
		s.reconcileOne(ctx, inst, byInstanceID[inst.ID], now)
	}

	for instanceID := range byInstanceID {
		if seen[instanceID] {
			continue
		}
		// Do not delete. A container the panel made whose row is gone is what
		// io.valmin.managed is for: it is surfaced for adoption, and removing it would
		// destroy a running server to tidy a table.
		c := byInstanceID[instanceID]
		slog.WarnContext(ctx, "orphaned container: managed by this panel, no instance row",
			slog.String("container_id", c.ID), slog.String("instance_id", instanceID),
			slog.Bool("running", c.Running))
	}
	return nil
}

// stream ties a log reader and a stats sampler to the lifetime of a running container. The
// ring buffer the reader filled outlives both, which is what leaves a stopped server's
// console still showing why it stopped.
//
// ctx is taken and deliberately not passed on: the reader must outlive the pass that
// noticed the container, and one cancelled with the reconcile context would close every
// console ten seconds after opening it.
func (s *Supervisor) stream(_ context.Context, instanceID string, c *runtime.Container) {
	if c != nil && c.Running {
		//nolint:contextcheck // see above: the reader outlives this pass on purpose
		s.inst.Streams.Open(instanceID, c.ID)
		return
	}
	s.inst.Streams.Close(instanceID)
}

// publish announces a transition the observer made, after the write it announces. Nil-safe:
// a Supervisor built for a test without a hub simply announces nothing.
func (s *Supervisor) publish(instanceID, state string, restartRequired bool) {
	if s.hub != nil {
		s.hub.PublishState(instanceID, state, restartRequired)
	}
}

// managedContainers is every container this panel created, keyed by the instance id its
// label carries. The join is on the label, never on instances.container_id, which is what
// lets the panel find its containers after the database is deleted and recreated (A2).
func (s *Supervisor) managedContainers(ctx context.Context) (map[string]*runtime.Container, error) {
	containers, err := s.inst.Runtime.List(ctx, map[string]string{instance.LabelManaged: "true"})
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	byInstanceID := make(map[string]*runtime.Container, len(containers))
	for i := range containers {
		c := &containers[i]
		id := c.Labels[instance.LabelInstanceID]
		if id == "" {
			slog.WarnContext(ctx, "managed container carries no instance id label",
				slog.String("container_id", c.ID))
			continue
		}
		byInstanceID[id] = c
	}
	return byInstanceID, nil
}

// reconcileOne applies one Verdict. Every write here is the observer's, the second of
// instances.state's two permitted writers, and it only ever runs for an instance whose lock
// is free.
func (s *Supervisor) reconcileOne(ctx context.Context, inst *store.Instance, c *runtime.Container, now time.Time) {
	reality := instance.Reality{}
	containerID := ""
	if c != nil {
		containerID = c.ID
		reality = instance.Reality{
			Found: true, Running: c.Running, OOMKilled: c.OOMKilled, RestartCount: c.RestartCount,
			CrashLooping: s.crash.Looping(c.ID, c.RestartCount, now),
		}
	}
	if c != nil && (inst.ContainerID == nil || *inst.ContainerID != c.ID) {
		// Docker wins. The label join already found the truth; the column is what is stale,
		// and leaving it stale leaves every later start pointed at nothing.
		if err := s.inst.DB.SetInstanceContainerID(ctx, inst.ID, c.ID); err != nil {
			slog.WarnContext(ctx, "repointing instance at its real container",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
		}
	}

	verdict := instance.Observe(instance.State(inst.State), reality)
	if verdict == (instance.Verdict{}) {
		return
	}

	if verdict.Rerun != (jobs.Kind{}) {
		if err := s.rerun(ctx, inst, verdict.Rerun); err == nil {
			slog.InfoContext(ctx, "re-submitted an interrupted job",
				slog.String("instance_id", inst.ID), slog.String("kind", verdict.Rerun.String()))
			return
		} else if verdict.To == "" {
			slog.WarnContext(ctx, "interrupted job could not be re-submitted",
				slog.String("instance_id", inst.ID), slog.String("kind", verdict.Rerun.String()),
				slog.Any("error", err))
			return
		}
	}

	to := verdict.To
	if verdict.Recheck {
		to = s.recheckReadiness(ctx, inst.ID, containerID)
	}
	if verdict.Stop && containerID != "" {
		// `unless-stopped` will resurrect a container the panel wants parked, so parking the
		// row is only half of it. An OOM-kill is a SIGKILL and therefore probable world
		// damage — restarting into the same limit corrupts the world again on a timer, which
		// is why this is never silently auto-healed.
		if err := s.inst.Runtime.Stop(ctx, containerID, "SIGINT", s.inst.Cfg.Game.StopTimeout.Std()); err != nil {
			slog.WarnContext(ctx, "stopping a container the panel is parking in error",
				slog.String("container_id", containerID), slog.Any("error", err))
		}
	}

	if _, err := s.inst.DB.UpdateInstanceState(ctx, inst.ID, inst.State, string(to)); err != nil {
		slog.WarnContext(ctx, "observer could not write instance state",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return
	}
	s.publish(inst.ID, string(to), inst.RestartRequired)
	slog.InfoContext(ctx, "reconciled instance",
		slog.String("instance_id", inst.ID), slog.String("from", inst.State),
		slog.String("to", string(to)), slog.String("reason", verdict.Reason))
}

// recheckReadiness reports whether readiness can be re-established for a container that
// outlived the process that started it.
//
// Settle is zero, not jobs.ready_settle. This container has been up since before the crash,
// so the readiness line is either already in its log or it is not, and waiting fifteen
// seconds per instance would stall the daemon's own startup to re-ask a question the log
// has already answered. A missing line is still not a failure (E6); only an exited
// container is.
func (s *Supervisor) recheckReadiness(ctx context.Context, instanceID, containerID string) instance.State {
	confirmed, err := instance.AwaitReady(ctx, s.inst.Runtime, containerID, 0, s.inst.Cfg.Jobs.ReadyTimeout.Std())
	if err != nil {
		slog.WarnContext(ctx, "readiness could not be re-established after a crash",
			slog.String("instance_id", instanceID), slog.Any("error", err))
		return instance.StateError
	}
	if !confirmed {
		slog.InfoContext(ctx, "instance is running with its backend registration unconfirmed",
			slog.String("instance_id", instanceID))
	}
	return instance.StateRunning
}

// errNoResume reports that no checkpoint exists, so the verdict's fallback state applies
// instead.
var errNoResume = errors.New("no resumable job")

func (s *Supervisor) rerun(ctx context.Context, inst *store.Instance, kind jobs.Kind) error {
	last, err := s.inst.DB.LastJobForInstance(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read last job for instance %s: %w", inst.ID, err)
	}
	switch kind {
	case jobs.KindProvision:
		return s.rerunProvision(ctx, inst, last)
	case jobs.KindDelete:
		return s.rerunDelete(ctx, inst, last)
	default:
		return fmt.Errorf("no re-run defined for kind %s", kind)
	}
}

// rerunProvision resumes an interrupted provision if a checkpoint exists, else the caller
// parks the instance in `error`.
//
// The checkpoint is a permission, not a position. Every provision phase is idempotent —
// EnsureBuildCached and CloneWithProgress each skip work already done, and SteamCMD itself
// resumes — so the resumed job re-runs from the top and converges. What the checkpoint
// decides is whether a resume is warranted at all: a provision that died before writing
// even its first one has proven nothing about its ability to make progress, and retrying it
// on every boot would be a loop.
func (s *Supervisor) rerunProvision(ctx context.Context, inst *store.Instance, last *store.Job) error {
	if last == nil || last.Kind != jobs.KindProvision.String() || last.Checkpoint == nil {
		return errNoResume
	}
	password, err := s.inst.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.Location{Table: "instances", Column: "password", RowID: inst.ID},
		mustReadPassword(ctx, s.inst.DB, inst.ID),
	)
	if err != nil {
		return fmt.Errorf("decrypt password for instance %s: %w", inst.ID, err)
	}
	var payload provisionPayload
	if err := json.Unmarshal([]byte(last.Payload), &payload); err != nil {
		return fmt.Errorf("decode provision payload of job %s: %w", last.ID, err)
	}

	run := &provisionRun{
		instanceID: inst.ID, name: inst.Name, basePort: inst.BasePort, dataDir: inst.DataDir,
		serverName: inst.ServerName, worldName: inst.WorldName, password: string(password),
		public: inst.Public, crossplay: inst.Crossplay, crossplayInstanceID: inst.CrossplayInstanceID,
		preset: deref(inst.Preset), modifiers: deref(inst.Modifiers), extraArgs: deref(inst.ExtraArgs),
		memLimitMB: inst.MemLimitMB, cpuLimit: inst.CPULimit,
		startAfterProvision: payload.StartAfterProvision,
		// The wizard's mods survive the crash with the rest of the instruction. This run may
		// be resuming before the install chain ever ran, and dropping them would provision
		// the server, start it, and generate the world unmodded.
		mods: payload.Mods,
		// No requestedBy: the panel is resuming this on its own behalf, and attributing it
		// to whoever clicked Create would be a lie in the audit trail.
	}
	if _, err := s.inst.submitProvision(ctx, run, instance.StateProvisioning); err != nil {
		return fmt.Errorf("resume provision for instance %s: %w", inst.ID, err)
	}
	return nil
}

// rerunDelete re-runs an interrupted delete, which is idempotent.
//
// keep_worlds comes off the dead job's own payload and defaults to true whenever it cannot
// be read. The panel never removes worlds/ outside a delete job explicitly told to, and a
// job row that will not parse is not "explicitly told to".
func (s *Supervisor) rerunDelete(ctx context.Context, inst *store.Instance, last *store.Job) error {
	keepWorlds := true
	if last != nil && last.Kind == jobs.KindDelete.String() {
		var payload deletePayload
		if err := json.Unmarshal([]byte(last.Payload), &payload); err != nil {
			slog.WarnContext(ctx, "delete payload unreadable, keeping worlds",
				slog.String("job_id", last.ID), slog.Any("error", err))
		} else {
			keepWorlds = payload.KeepWorlds
		}
	}
	if _, err := s.inst.submitDelete(ctx, inst, keepWorlds, ""); err != nil {
		return fmt.Errorf("re-run delete for instance %s: %w", inst.ID, err)
	}
	return nil
}

// mustReadPassword reads the stored envelope, answering "" on a failure the Decrypt call
// then reports — this keeps the resume path's one error message about the secret rather than
// about the read that fetched it.
func mustReadPassword(ctx context.Context, db *store.DB, instanceID string) string {
	envelope, err := db.InstancePassword(ctx, instanceID)
	if err != nil {
		return ""
	}
	return envelope
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orphans handles GET /instances/orphans: the containers this panel created that no
// instance row claims. They are reported, never removed.
//
// Gated on panel.settings, which is admin-only and never grantable. An orphan is a
// panel-wide fact about the host rather than an instance-scoped one, so no grant could
// scope it — and it exposes container ids and ports, which stay with admins (D15).
func (h *Instances) orphans(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	found, err := NewSupervisor(h).Orphans(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(found, nil))
}

// Orphan is a container carrying this panel's labels that no instance row claims.
type Orphan struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	InstanceID  string `json:"instance_id"`
	BasePort    int    `json:"base_port"`
	Running     bool   `json:"running"`
}

// Orphans lists them, newest information first from Docker's own ordering.
func (s *Supervisor) Orphans(ctx context.Context) ([]Orphan, error) {
	byInstanceID, err := s.managedContainers(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.inst.DB.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list orphans: %w", err)
	}
	claimed := make(map[string]bool, len(instances))
	for i := range instances {
		claimed[instances[i].ID] = true
	}

	orphans := []Orphan{}
	for instanceID := range byInstanceID {
		if claimed[instanceID] {
			continue
		}
		c := byInstanceID[instanceID]
		basePort, _ := strconv.Atoi(c.Labels[instance.LabelBasePort])
		orphans = append(orphans, Orphan{
			ContainerID: c.ID, Name: c.Name, InstanceID: instanceID,
			BasePort: basePort, Running: c.Running,
		})
	}
	return orphans, nil
}
