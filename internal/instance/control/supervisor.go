package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// observeInterval is how often the observer asks Docker what happened. Polling beats an
// event bus for a handful of containers: one labelled List every ten seconds costs less
// than reconnect handling, and is well inside the time anyone takes to notice an outage.
const observeInterval = 10 * time.Second

// Supervisor owns crash recovery and reconciliation of observed container state.
type Supervisor struct {
	DB                   *store.DB
	Engine               *jobs.Engine
	Runtime              runtime.Runtime
	Keeper               *crypto.Keeper
	Streams              *instance.Streams
	DataRoot             string
	HostRoot             string
	StopTimeout          time.Duration
	ReadyTimeout         time.Duration
	PublishState         func(string, string, bool)
	NotifyUnexpectedStop func(context.Context, *store.Instance, string, string)
	SubmitStart          func(context.Context, *store.Instance, string) error
	SubmitUpdateCheck    func(context.Context, string) error
	SubmitProvision      func(context.Context, *ProvisionRun) error
	SubmitDelete         func(context.Context, *store.Instance, bool) error
	crash                *instance.CrashLoop
	owedStops            map[string]string
}

func (s *Supervisor) initialize() {
	if s.crash == nil {
		s.crash = instance.NewCrashLoop()
	}
	if s.owedStops == nil {
		s.owedStops = make(map[string]string)
	}
}

// Recover runs the sweep, then the reconcile, then the resume intents, in that order and no
// other: sweeping first is what leaves the reconciler only unlocked instances to judge (C6).
//
// The startup gate and the daemon lease are the caller's. Log streams re-open as a side effect
// of the reconcile pass, which opens a reader for every running container it finds.
func (s *Supervisor) Recover(ctx context.Context) error {
	s.initialize()
	s.sweepThrowaways(ctx)
	resume, err := s.Sweep(ctx)
	if err != nil {
		return err
	}
	if referenced, err := s.DB.ReferencedSetupArtifacts(ctx); err != nil {
		slog.WarnContext(ctx, "list setup artifacts for cleanup", slog.Any("error", err))
	} else if err := setupblob.New(s.DataRoot).GC(referenced); err != nil {
		slog.WarnContext(ctx, "clean up unused setup artifacts", slog.Any("error", err))
	}
	if err := s.Reconcile(ctx); err != nil {
		return err
	}
	s.resumeIntents(ctx, resume)
	s.interruptOperations(ctx)
	return nil
}

// sweepThrowaways removes the one-shot helpers a killed panel left behind. Nothing reads a
// throwaway's result but the call that created it, and that call's process is gone, so a
// survivor is at best a stopped container nobody will ever look at and at worst a SteamCMD
// still writing into a bind mount this panel is about to write itself (08 §3.2).
//
// A failure here is logged, never fatal: leftover containers are untidy, and refusing to start
// the panel over them would be worse than the leak.
func (s *Supervisor) sweepThrowaways(ctx context.Context) {
	n, err := runtime.RemovePanelThrowaways(ctx, s.Runtime, s.HostRoot)
	if err != nil {
		slog.ErrorContext(ctx, "could not remove leftover throwaway containers", slog.Any("error", err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "removed throwaway containers left by an earlier run", slog.Int("count", n))
	}
}

// interruptOperations marks every definition chain the crash cut as interrupted. It runs last,
// so a chain whose provision job reconciliation just resubmitted stays running; everything
// else is left for an operator to resume or abandon explicitly, never replayed here (Q52).
func (s *Supervisor) interruptOperations(ctx context.Context) {
	n, err := s.DB.InterruptIdleOperations(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "mark interrupted definition operations", slog.Any("error", err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "definition operations interrupted by the last shutdown",
			slog.Int("count", n))
	}
}

// Run is the observer loop: the same reconciliation pass, on a timer, for the life of the
// process. It returns when ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	s.initialize()
	ticker := time.NewTicker(observeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The readers outlive any one request context by design, so shutdown is the one
			// place that has to end them.
			s.Streams.Shutdown()
			return
		case <-ticker.C:
			if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "observer pass failed, will retry", slog.Any("error", err))
			}
		}
	}
}

// sweep closes out every row still marked `running` whose lease_owner is not this boot's,
// those belonging to a dead process. It returns the instance ids whose swept job carried a
// resume intent this build is allowed to honour.
//
// No kind is continued in place: every swept row is closed out as `interrupted` with its lock
// released. Read-only update checks are resubmitted; instance jobs defer to reconciliation.
func (s *Supervisor) Sweep(ctx context.Context) (resume []string, err error) {
	stale, err := s.DB.StaleJobs(ctx, s.Engine.Owner())
	if err != nil {
		return nil, fmt.Errorf("sweep dead jobs: %w", err)
	}
	code := errcode.Interrupted.String()
	message := "The panel stopped while this job was running."
	for i := range stale {
		j := &stale[i]
		if err := s.DB.FinishJob(
			ctx, j.ID, jobs.StatusFailed, j.Progress, &code, &message, j.Log, j.Clean, time.Now(), nil,
		); err != nil {
			return nil, fmt.Errorf("sweep dead job %s: %w", j.ID, err)
		}
		slog.InfoContext(ctx, "swept dead job",
			slog.String("job_id", j.ID), slog.String("kind", j.Kind),
			slog.String("instance_id", deref(j.InstanceID)), slog.Any("checkpoint", j.Checkpoint))

		s.sweepStaging(ctx, j)

		kind, known := jobs.ByName(j.Kind)
		if kind == jobs.KindUpdateCheck && j.InstanceID == nil {
			if err := s.SubmitUpdateCheck(ctx, deref(j.ScheduleID)); err != nil {
				return nil, fmt.Errorf("resume interrupted update check: %w", err)
			}
		}
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
	recovery := &Recovery{DB: s.DB, Runtime: s.Runtime, DataRoot: s.DataRoot}
	modRecovery := &manager.Recovery{DB: s.DB, DataRoot: s.DataRoot}
	switch j.Kind {
	case jobs.KindWorldImport.String():
		recovery.SweepImportStaging(ctx, j)
	case jobs.KindModInstall.String():
		modRecovery.SweepModInstall(ctx, j)
	case jobs.KindModUninstall.String():
		modRecovery.SweepModUninstall(ctx, j)
	case jobs.KindModToggle.String():
		modRecovery.SweepModToggle(ctx, j)
	case jobs.KindBackup.String():
		recovery.SweepBackupPart(ctx, j)
	case jobs.KindRestore.String():
		recovery.SweepRestoreSwap(ctx, j)
	case jobs.KindGameUpdate.String():
		recovery.SweepUpdateSwap(ctx, j)
	case jobs.KindClone.String():
		recovery.SweepCloneStaging(ctx, j)
	case jobs.KindSetupSave.String():
		recovery.SweepSetupSave(ctx, j)
	case jobs.KindSetupRestore.String():
		recovery.SweepSetupRestore(ctx, j)
	}
}

// resumeIntents re-submits the starts a crash interrupted. It runs after reconciliation,
// not before: an instance owes the user a restart only once the matrix has resolved it to a
// state a start can be claimed from.
func (s *Supervisor) resumeIntents(ctx context.Context, instanceIDs []string) {
	for _, id := range instanceIDs {
		inst, err := s.DB.InstanceByID(ctx, id)
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
		if err := s.SubmitStart(ctx, inst, *inst.ContainerID); err != nil {
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
func (s *Supervisor) Reconcile(ctx context.Context) error {
	held, err := s.DB.HeldLockKeys(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	instances, err := s.DB.ListInstances(ctx, nil)
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
		// A held lock means a job is making an intentional change, so the exit it causes must not
		// be observed as an unexpected one (C14).
		if held[jobs.InstanceLockKey(inst.ID)] {
			continue
		}
		s.ReconcileOne(ctx, inst, byInstanceID[inst.ID], now)
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

// stream ties a log reader and a stats sampler to the lifetime of a running container. The ring
// buffer outlives both, leaving a stopped server's console still showing why it stopped.
//
// ctx is taken and deliberately not passed on: the reader must outlive the reconcile pass that
// noticed the container.
func (s *Supervisor) stream(_ context.Context, instanceID string, c *runtime.Container) {
	if c != nil && c.Running {
		s.Streams.Open(instanceID, c.ID) //nolint:contextcheck // the reader outlives this pass
		return
	}
	s.Streams.Close(instanceID)
}

// publish announces a transition the observer made, after the write it announces. Nil-safe:
// a Supervisor built for a test without a hub simply announces nothing.
func (s *Supervisor) publish(instanceID, state string, restartRequired bool) {
	if s.PublishState != nil {
		s.PublishState(instanceID, state, restartRequired)
	}
}

// managedContainers is every container this panel created, keyed by the instance id its
// label carries. The join is on the label, never on instances.container_id, which is what
// lets the panel find its containers after the database is deleted and recreated (A2).
func (s *Supervisor) managedContainers(ctx context.Context) (map[string]*runtime.Container, error) {
	return ManagedContainers(ctx, s.Runtime)
}

// reconcileOne applies one Verdict. Every write here is the observer's, the second of
// instances.state's two permitted writers, and it only ever runs for an instance whose lock
// is free.
func (s *Supervisor) ReconcileOne(ctx context.Context, inst *store.Instance, c *runtime.Container, now time.Time) {
	s.initialize()
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
		if err := s.DB.SetInstanceContainerID(ctx, inst.ID, c.ID); err != nil {
			slog.WarnContext(ctx, "repointing instance at its real container",
				slog.String("instance_id", inst.ID), slog.Any("error", err))
		}
	}

	verdict := s.stillOwed(containerID, reality, instance.Observe(instance.State(inst.State), reality))
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
	if !s.park(ctx, inst, containerID, &verdict) {
		return
	}

	// The compare-and-swap result, not just the error. HeldLockKeys was read at the top of the
	// pass, so a job can have taken the lock and moved the row since: the update then matches
	// nothing, and announcing a transition that did not commit tells every console a server
	// changed state when it did not (C13).
	written, err := instance.SetState(ctx, s.DB, inst.ID, instance.State(inst.State), to)
	if err != nil {
		slog.WarnContext(ctx, "observer could not write instance state",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return
	}
	if !written {
		slog.InfoContext(ctx, "instance moved under the observer; leaving it to the next pass",
			slog.String("instance_id", inst.ID), slog.String("from", inst.State))
		return
	}
	s.publish(inst.ID, string(to), inst.RestartRequired)
	s.notifyIfDown(ctx, inst, string(to), verdict.Reason)
	slog.InfoContext(ctx, "reconciled instance",
		slog.String("instance_id", inst.ID), slog.String("from", inst.State),
		slog.String("to", string(to)), slog.String("reason", verdict.Reason))
}

// stillOwed re-raises a protective stop an earlier pass decided on and could not carry out.
//
// The obligation cannot be re-derived from reality, which is what it looked like it could be:
// the evidence expires and the container does not. CrashLoop rebases its baseline once the
// window has elapsed, so a stop that failed at minute one is, at minute eleven, a container
// still restarting that nothing asks about any more. It is therefore remembered against the
// container until the container is no longer running — by this panel's stop, by the operator,
// or by the server exiting on its own (08 §6).
func (s *Supervisor) stillOwed(
	containerID string, reality instance.Reality, verdict instance.Verdict,
) instance.Verdict {
	if containerID == "" {
		return verdict
	}
	reason, owed := s.owedStops[containerID]
	if !owed {
		return verdict
	}
	if !reality.Found || !reality.Running {
		delete(s.owedStops, containerID)
		return verdict
	}
	verdict.Stop = true
	if verdict.To == "" {
		verdict.To, verdict.Reason = instance.StateError, reason
	}
	return verdict
}

// park carries out a verdict's protective stop and reports whether the caller may go on to
// write the new state.
//
// `unless-stopped` would resurrect a container the panel wants parked, so parking it is only
// half the job. An OOM-kill is a SIGKILL and therefore probable world damage; restarting into
// the same limit would repeat it, so this is never auto-healed.
//
// `↯` A stop that fails leaves the row where it is. Writing `error` anyway would settle the
// matter permanently — `error` is a state the observer never leaves (12 §2.4), so nothing would
// try again — while the container went on restarting. What is recorded instead is the
// obligation, which stillOwed re-raises until the container stops: containment outlives the
// evidence that justified it, and none of it is a retried write over world data (B13).
func (s *Supervisor) park(
	ctx context.Context, inst *store.Instance, containerID string, verdict *instance.Verdict,
) bool {
	if !verdict.Stop || containerID == "" {
		return true
	}
	err := s.Runtime.Stop(ctx, containerID, "SIGINT", s.StopTimeout)
	if err != nil {
		s.owedStops[containerID] = verdict.Reason
		slog.WarnContext(ctx, "protective stop failed; the stop stays owed until the container is down",
			slog.String("instance_id", inst.ID), slog.String("container_id", containerID),
			slog.String("reason", verdict.Reason), slog.Any("error", err))
		return false
	}
	delete(s.owedStops, containerID)
	return true
}

// notifyIfDown owes a notification when a server the operator expects to be live is not. A
// transition out of a live state, observed with no job holding the instance lock, is a server
// that went down on its own (C14) — an operator's own stop holds that lock and never reaches
// the observer, which is what keeps an expected stop quiet.
func (s *Supervisor) notifyIfDown(ctx context.Context, inst *store.Instance, to, reason string) {
	if !wasUp(inst.State) || wasUp(to) {
		return
	}
	// Recorded as well as announced: a crash loop is a rate, and an instance that crashes and
	// restarts between two condition scans is never observed down.
	if err := s.DB.RecordIncident(ctx, inst.ID, reason, time.Now().UTC()); err != nil {
		slog.WarnContext(ctx, "record unexpected stop",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
	}
	if s.NotifyUnexpectedStop == nil {
		return
	}
	s.NotifyUnexpectedStop(ctx, inst, to, reason)
}

// wasUp reports whether a state is one the operator expects a live server in.
func wasUp(state string) bool {
	return state == string(instance.StateRunning) || state == string(instance.StateStarting)
}

// recheckReadiness reports whether readiness can be re-established for a container that
// outlived the process that started it. Settle is zero rather than jobs.ready_settle: the
// container predates the crash, so its log has already answered the question. A missing line is
// still not a failure, only an exited container is (E6).
func (s *Supervisor) recheckReadiness(ctx context.Context, instanceID, containerID string) instance.State {
	confirmed, err := instance.AwaitReady(ctx, s.Runtime, containerID, 0, s.ReadyTimeout)
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Supervisor) rerun(ctx context.Context, inst *store.Instance, kind jobs.Kind) error {
	last, err := s.DB.LastJobForInstance(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read last job for instance %s: %w", inst.ID, err)
	}
	switch kind {
	case jobs.KindProvision:
		if last != nil && last.Kind == jobs.KindClone.String() {
			return errNoResume
		}
		return s.resumeProvision(ctx, inst, last)
	case jobs.KindDelete:
		return s.resumeDelete(ctx, inst, last)
	default:
		return fmt.Errorf("no re-run defined for kind %s", kind)
	}
}

// resumeProvision replays idempotent phases only after the old job reached a checkpoint.
func (s *Supervisor) resumeProvision(ctx context.Context, inst *store.Instance, last *store.Job) error {
	if last == nil || last.Kind != jobs.KindProvision.String() || last.Checkpoint == nil {
		return errNoResume
	}
	envelope, err := s.DB.InstancePassword(ctx, inst.ID)
	if err != nil {
		// Preserve the original decrypt error path when the envelope cannot be read.
		envelope = ""
	}
	password, err := s.Keeper.Decrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(inst.ID),
		envelope,
	)
	if err != nil {
		return fmt.Errorf("decrypt password for instance %s: %w", inst.ID, err)
	}
	var payload ProvisionPayload
	if err := json.Unmarshal([]byte(last.Payload), &payload); err != nil {
		return fmt.Errorf("decode provision payload of job %s: %w", last.ID, err)
	}
	run := &ProvisionRun{
		InstanceID: inst.ID, Name: inst.Name, BasePort: inst.BasePort, DataDir: inst.DataDir,
		ServerName: inst.ServerName, WorldName: inst.WorldName, Password: string(password),
		Public: inst.Public, Crossplay: inst.Crossplay, CrossplayInstanceID: inst.CrossplayInstanceID,
		Preset: deref(inst.Preset), Modifiers: deref(inst.Modifiers), ExtraArgs: deref(inst.ExtraArgs),
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
		StartAfterProvision: payload.StartAfterProvision,
	}
	if err := s.SubmitProvision(ctx, run); err != nil {
		return fmt.Errorf("resume provision for instance %s: %w", inst.ID, err)
	}
	return nil
}

// resumeDelete keeps worlds unless the interrupted job explicitly requested their removal.
func (s *Supervisor) resumeDelete(ctx context.Context, inst *store.Instance, last *store.Job) error {
	keepWorlds := true
	if last != nil && last.Kind == jobs.KindDelete.String() {
		var payload DeletePayload
		if err := json.Unmarshal([]byte(last.Payload), &payload); err != nil {
			slog.WarnContext(ctx, "delete payload unreadable, keeping worlds",
				slog.String("job_id", last.ID), slog.Any("error", err))
		} else {
			keepWorlds = payload.KeepWorlds
		}
	}
	if err := s.SubmitDelete(ctx, inst, keepWorlds); err != nil {
		return fmt.Errorf("re-run delete for instance %s: %w", inst.ID, err)
	}
	return nil
}
