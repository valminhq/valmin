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

// observeInterval is how often the observer asks Docker what happened. 01 §6 prefers polling
// to an event bus until measurement says otherwise, and this is a friend-group panel with a
// handful of containers: one labelled List every ten seconds costs less than an event
// stream's reconnect handling, and ten seconds is well inside the time it takes anyone to
// notice a server went down.
const observeInterval = 10 * time.Second

// Supervisor is 12 §1's second writer of instances.state — the observer, which records what
// Docker did without being asked — and 12 §9.1's startup sequence.
//
// `↯` It lives in internal/api despite serving no HTTP, because 12 §9.2's `provisioning` and
// `deleting` rows do not merely set a state: they re-submit the job. Their Runners are the
// ones POST /instances and DELETE /instances/{id} build, and ADR-028's whole claim is that
// there is one reliability story per operation — so the recovery path runs the identical
// code rather than a second copy that drifts. A package of its own would have to export
// every Runner to get there.
type Supervisor struct {
	inst  *Instances
	crash *instance.CrashLoop
	// hub is 14 §4.4's other publisher: the observer writes instances.state too, and a
	// transition nobody announced is a dashboard that stays wrong until the next reload.
	hub *ws.Hub
}

// NewSupervisor builds the observer over the same dependencies the instance handlers hold.
func NewSupervisor(inst *Instances) *Supervisor {
	return &Supervisor{inst: inst, crash: instance.NewCrashLoop()}
}

// Recover is 12 §9.1 steps 2 to 4, in that order and no other.
//
// `↯` The sweep precedes the reconcile (C6). Reconciling first means meeting an instance in
// a transient state whose lock is held by a process that no longer exists, and having to
// reason about whether to touch it; sweeping first means the reconciler only ever sees
// unlocked instances, which is the case 08 §6.1 was written for.
//
// Step 1 (the startup gate and the daemon lease) is the caller's. Step 5, re-opening the
// log streams, falls out of the reconcile pass: it opens a reader for every container it
// finds running, on this pass and on every one after.
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
			// place that has to end them (11 §10 closes the sockets that were reading).
			s.inst.Streams.Shutdown()
			return
		case <-ticker.C:
			if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "observer pass failed, will retry", slog.Any("error", err))
			}
		}
	}
}

// sweep is 12 §9.1 step 2: every row still marked `running` whose lease_owner is not this
// boot's belongs to a dead process. It returns the instance ids whose swept job carried a
// resume intent this build is allowed to honour (ADR-032).
//
// `↯` No M1 kind is continued in place. 12 §9.4 gives start, stop and restart no checkpoints
// at all, makes delete idempotent, and resumes provision from its checkpoint — but that
// resume is a fresh job, submitted by step 3's matrix once the lock is free. So every swept
// row is closed out as `interrupted` with its lock released, and what happens next is
// reconciliation's decision, not the sweep's.
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

// sweepImportStaging removes the upload directory a killed world import left behind —
// 12 §9.4's "partial staging deleted on failure", for the one case the import job could not
// handle itself because it was not running when the panel died.
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

// sweepModInstall is 12 §9.4's "not resumed — roll back from the manifest". The manifest is
// written before any file moves, so on a crash it is the exact record of what the job was
// going to place, whether or not it got there; the staging area holds the originals of
// everything it displaced. Together they return server/ to where it was.
//
// `↯` The packages to undo are read from the staging directory, not from the payload: the
// payload names only what the user asked for, and the closure the resolver produced from it
// is what actually got staged. A row exists for a staged package only because *this* job
// wrote it — an install skips a package already present at a satisfying version and refuses
// one present at a different version — so deleting those rows never touches another
// install's.
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

// sweepModUninstall is mod_uninstall's half of 12 §9.4's "not resumed — rolls back". The
// job saves every file it is going to remove before it removes any of them, and deletes the
// rows only in its own Finish transaction — so an interrupted uninstall still has its rows,
// and restoring the files from the backup is the whole of the recovery.
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

// withinRoot reports whether path is inside root. `↯` Not decoration: the paths it guards
// come out of a database column and feed a recursive delete running as the panel. 12 §10 is
// absolute that the panel never removes worlds/ outside a delete job that asked for it, and
// a payload naming somewhere else entirely gets nothing removed rather than the benefit of
// the doubt.
func withinRoot(root, path string) bool {
	within, err := filepath.Rel(root, path)
	return err == nil && within != "." && within != ".." &&
		!strings.HasPrefix(within, ".."+string(filepath.Separator))
}

// resumeIntents is 12 §9.1 step 4. It runs after reconciliation, not before: an instance
// owes the user a restart only once the matrix has resolved it to a state a start can be
// claimed from, and reconciliation is what does that.
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

// reconcile is 08 §6.1 steps 1 to 3: list the panel's containers, join them to the DB on the
// io.valmin.instance.id label, and resolve every disagreement. It is also the observer's
// steady-state pass, because those are the same question (internal/instance's Observe).
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
		// Before the lock check, deliberately: a reader is 14 §8's lifecycle and not a
		// state write, so C14 has nothing to say about it, and a server started by a job
		// should have its console open while that job is still running.
		s.stream(ctx, inst.ID, byInstanceID[inst.ID])
		// `↯` C14, and it is the whole reason this check is here rather than inside Observe:
		// while a lock is held there is a job making an intentional change, the container
		// will exit *because* that job stopped it, and an observer that writes on that event
		// races the job that caused it. 12 §1 calls this the single most likely concurrency
		// bug in the daemon.
		if held[jobs.InstanceLockKey(inst.ID)] {
			continue
		}
		s.reconcileOne(ctx, inst, byInstanceID[inst.ID], now)
	}

	for instanceID := range byInstanceID {
		if seen[instanceID] {
			continue
		}
		// `↯` Do not delete. A container the panel made, whose row is gone, is what
		// io.valmin.managed is for: it is surfaced for adoption (GET /instances/orphans),
		// and removing it would destroy a running server to tidy a table.
		c := byInstanceID[instanceID]
		slog.WarnContext(ctx, "orphaned container: managed by this panel, no instance row",
			slog.String("container_id", c.ID), slog.String("instance_id", instanceID),
			slog.Bool("running", c.Running))
	}
	return nil
}

// stream is 14 §8's source lifecycle: a log reader and a stats sampler exist exactly while a
// container runs, and the ring buffer the reader filled outlives both. Keeping the buffer
// rather than discarding it is what leaves a stopped server's console showing why it
// stopped — the most useful moment it has.
//
// `↯` ctx is taken and deliberately not passed on: the reader must outlive the pass that
// noticed the container, and a reader cancelled with the reconcile context would close every
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

// managedContainers is 08 §6.1 steps 1 and 2: every container this panel created, keyed by
// the instance id its label carries. The join is on the label, never on instances.container_id
// — which is what lets the panel find its containers after the database is deleted and
// recreated (A2, 08 §1).
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

// reconcileOne applies one Verdict. Every write here is the observer's, which is the second
// of instances.state's two permitted writers (12 §1) — and it only ever runs for an instance
// whose lock is free.
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
		// Docker wins (08 §6.1). The label join already found the truth; the column is what
		// is stale, and leaving it stale leaves every later start pointed at nothing.
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
		// 08 §6's guard: `unless-stopped` will resurrect a container the panel wants parked,
		// so parking the row is only half of it. An OOM-kill is a SIGKILL and therefore
		// probable world damage (03 §3.3) — restarting into the same limit corrupts the
		// world again on a timer, which is why this is never silently auto-healed.
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

// recheckReadiness answers 12 §9.2's `starting` + running row: whether readiness can be
// re-established for a container that outlived the process that started it.
//
// `↯` Settle is zero, not jobs.ready_settle. This container has been up since before the
// crash, so the readiness line is either already in its log or it is not — waiting fifteen
// seconds per instance would stall the daemon's own startup to re-ask a question the log has
// already answered. A missing line is still not a failure (ADR-043, E6): only an exited
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

// errNoResume reports that 12 §9.2's "if a checkpoint exists" does not hold — the verdict's
// fallback state applies instead.
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

// rerunProvision is 12 §9.2's `provisioning` row: resume from checkpoint if one exists, else
// the caller parks the instance in `error`.
//
// `↯` The checkpoint is a permission, not a position. Every provision phase is idempotent by
// construction (WP-13: EnsureBuildCached and CloneWithProgress each skip work already done,
// and SteamCMD itself resumes — Q22, measured 20 Aug 2026), so the resumed job simply re-runs
// from the top and converges. What the checkpoint decides is whether a resume is warranted
// at all: a provision that died before writing even its first one has proven nothing about
// its own ability to make progress, and retrying it on every boot would be a loop.
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
		// The wizard's mods survive the crash with the rest of the instruction (Q42). They
		// have to: this run may be resuming *before* the install chain ever ran, and a
		// resume that dropped them would provision the server, start it, and generate the
		// world vanilla — the exact ordering the feature exists to protect.
		mods: payload.Mods,
		// No requestedBy: the panel is resuming this on its own behalf, and attributing it
		// to whoever happened to click Create days ago would be a lie in the audit trail.
	}
	if _, err := s.inst.submitProvision(ctx, run, instance.StateProvisioning); err != nil {
		return fmt.Errorf("resume provision for instance %s: %w", inst.ID, err)
	}
	return nil
}

// rerunDelete is 12 §9.2's `deleting` row: re-run the delete, it is idempotent.
//
// `↯` keep_worlds comes off the dead job's own payload, and defaults to true whenever it
// cannot be read. The panel never removes worlds/ outside a delete job that was explicitly
// told to (12 §10) — and a job row that will not parse is not "explicitly told to".
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

// orphans is GET /instances/orphans (08 §6.1 step 3's last bullet): the containers this
// panel created that no instance row claims. Adoption itself is M5; M1's obligation is that
// they are reported rather than removed.
//
// `↯` Gated on panel.settings, which is admin-only and never grantable (09 §3.3). An orphan
// is a panel-wide fact about the host, not an instance-scoped one, so there is no grant that
// could scope it — and it exposes container ids and ports, which D15 keeps to admins.
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

// Orphan is a container carrying this panel's labels that no instance row claims (08 §6.1
// step 3's last bullet). M1 surfaces them; the adopt action is M5.
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
