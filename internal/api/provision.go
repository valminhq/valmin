package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// createInstanceRequest is POST /instances' body (04 §3, WP-M1-13). Every field it accepts
// is admin-only by construction — the whole endpoint is gated on instance.create (09 §3.3),
// so unlike PATCH there is no separate per-field action to check (ADR-061).
type createInstanceRequest struct {
	Name                string            `json:"name"`
	ServerName          string            `json:"server_name"`
	WorldName           string            `json:"world_name"`
	Password            string            `json:"password"`
	Public              bool              `json:"public"`
	Crossplay           bool              `json:"crossplay"`
	Preset              string            `json:"preset,omitempty"`
	Modifiers           map[string]string `json:"modifiers,omitempty"`
	MemLimitMB          int               `json:"mem_limit_mb,omitempty"`
	CPULimit            *float64          `json:"cpu_limit,omitempty"`
	ExtraArgs           string            `json:"extra_args,omitempty"`
	StartAfterProvision bool              `json:"start_after_provision,omitempty"`
	// Mods are installed once provisioning succeeds and before start_after_provision
	// starts anything (Q42). Empty is the vanilla create this endpoint has always been.
	Mods []resolveRequest `json:"mods,omitempty"`
}

// provisionPayload is the provision job's persisted payload (ADR-033):
// start_after_provision lives here, not as an instances column, because it is an
// instruction for this one run, not a durable fact about the instance. `↯` Not yet acted
// on — the runner below stores it and stops; the start job that would read it is WP-14's.
type provisionPayload struct {
	StartAfterProvision bool `json:"start_after_provision"`
	// Mods is what the wizard asked to have installed before the first boot. On the payload
	// rather than in a table for the same reason StartAfterProvision is: an instruction for
	// this one run, not a durable fact about the instance — instance_mods records what
	// actually landed.
	Mods []resolveRequest `json:"mods,omitempty"`
}

// provisionBuildID stands in for real Steam build-id detection (`appmanifest_896660.acf`,
// 08 §7), which is M4's game_update. M1 has exactly one cache entry, always re-validated
// in place — flagged as Q29 rather than silently assumed permanent.
const provisionBuildID = "latest"

const maxPortAllocationAttempts = 3

// create is POST /instances (04 §3): admin-only, returns 202 and a provision job, never the
// instance itself (11 §3) — the row exists in `created` the moment this returns, but
// nothing about it is real on disk or in Docker until the job's Finish phase says so.
func (h *Instances) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}

	var body createInstanceRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	var val apierr.Validation
	if body.Name == "" {
		val.Add("name", apierr.FieldRequired, "Name is required.")
	}
	for _, v := range instance.ValidateLaunch(body.ServerName, body.WorldName, body.Password) {
		addLaunchViolation(&val, v)
	}
	modifiers, modErr := encodeModifiers(body.Modifiers)
	if modErr != nil {
		val.Add("modifiers", apierr.FieldInvalid, "Modifiers must be a flat object of strings.")
	}
	validateModRequests(&val, body.Mods)
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	if !h.modsAreInstallable(w, r, body.Mods) {
		return
	}

	memLimitMB := body.MemLimitMB
	if memLimitMB == 0 {
		memLimitMB = h.Cfg.Game.DefaultMemMB
	}

	id := store.NewID()
	dataDir := h.Cfg.Data.HostRoot + "/instances/" + id
	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.Location{Table: "instances", Column: "password", RowID: id},
		[]byte(body.Password),
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	basePort, err := h.createInstanceRow(r.Context(), id, dataDir, envelope, modifiers, memLimitMB, &body)
	if err != nil {
		writeCreateInstanceError(w, r, err)
		return
	}

	job, err := h.submitProvision(r.Context(), &provisionRun{
		instanceID: id, name: body.Name, basePort: basePort, dataDir: dataDir,
		serverName: body.ServerName, worldName: body.WorldName, password: body.Password,
		public: body.Public, crossplay: body.Crossplay, crossplayInstanceID: id,
		preset: body.Preset, modifiers: modifiers, extraArgs: body.ExtraArgs,
		memLimitMB: memLimitMB, cpuLimit: body.CPULimit,
		startAfterProvision: body.StartAfterProvision, mods: body.Mods, requestedBy: u.ID,
	}, instance.StateCreated)
	if err != nil {
		var conflict *store.JobConflict
		if errors.As(err, &conflict) {
			apierr.Write(w, r, apierr.New(apierr.JobInProgress).With("job_id", conflict.JobID))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// validateModRequests keeps a malformed mod entry with the rest of the body's validation,
// so it answers 422 naming the field rather than reaching the resolver as a request for a
// package called "".
func validateModRequests(val *apierr.Validation, mods []resolveRequest) {
	for i, m := range mods {
		if strings.TrimSpace(m.FullName) == "" || strings.TrimSpace(m.Version) == "" {
			val.Add(fmt.Sprintf("mods[%d]", i), apierr.FieldRequired,
				"Each mod needs a full_name and a version.")
		}
	}
}

// modsAreInstallable is the create request's mod check, and it runs *before* the instance
// row and the port allocation — everything past that point is a resource to unwind.
//
// `↯` A mod nobody can resolve is the caller's mistake, and it is worth far more to them
// here, as a 409 naming the package, than as a job that fails after a 1 GB game download
// has already run (Q42). Resolved against an instance that does not exist yet: nothing is
// installed on it, which is exactly true of the one about to be. Writes the response and
// reports false when the request cannot go ahead.
func (h *Instances) modsAreInstallable(w http.ResponseWriter, r *http.Request, mods []resolveRequest) bool {
	if len(mods) == 0 {
		return true
	}
	if h.Mods == nil {
		apierr.Write(w, r, apierr.New(apierr.Unavailable).
			Wrap(errors.New("this panel has no mod engine, so mods cannot be installed at create")))
		return false
	}
	fresh := &store.Instance{}
	for _, req := range mods {
		if err := h.Mods.CheckResolvable(r.Context(), fresh, req); err != nil {
			writeResolveError(w, r, err)
			return false
		}
	}
	return true
}

// submitProvision claims `from → provisioning` and dispatches the provision job. from is
// `created` for POST /instances and `provisioning` for 12 §9.2's resume of a run whose
// process died, which finds the row already there — a self-transition the compare-and-swap
// accepts, so the resume needs no claim of its own.
func (h *Instances) submitProvision(
	ctx context.Context, run *provisionRun, from instance.State,
) (*store.Job, error) {
	id := run.instanceID
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind:         jobs.KindProvision,
		LockKey:      jobs.InstanceLockKey(id),
		InstanceID:   &id,
		InstanceName: run.name,
		RequestedBy:  run.requestedBy,
		Payload:      provisionPayload{StartAfterProvision: run.startAfterProvision, Mods: run.mods},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(from), string(instance.StateProvisioning))
			if err != nil {
				return fmt.Errorf("claim provision for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in %s state at claim", id, from)
			}
			return nil
		},
	}, h.runProvision(run))
	if err != nil {
		return nil, fmt.Errorf("submit provision for instance %s: %w", id, err)
	}
	return job, nil
}

// createInstanceRow allocates a port and inserts the row, retrying the allocation a few
// times if it loses a race with a concurrent create (store.ErrBasePortTaken) — the base
// port is the panel's own choice, not the caller's, so a collision here is a transient
// race rather than something to report back as a validation failure.
func (h *Instances) createInstanceRow(
	ctx context.Context, id, dataDir, envelope, modifiers string, memLimitMB int, body *createInstanceRequest,
) (basePort int, err error) {
	allocator := instance.NewAllocator(h.DB, h.Cfg.Ports.Base, h.Cfg.Ports.Stride)
	for attempt := 0; attempt < maxPortAllocationAttempts; attempt++ {
		basePort, err = allocator.Allocate(ctx)
		if err != nil {
			return 0, fmt.Errorf("allocate port: %w", err)
		}
		err = h.DB.CreateInstance(ctx, &store.NewInstance{
			ID: id, Name: body.Name, DataDir: dataDir, BasePort: basePort,
			ServerName: body.ServerName, WorldName: body.WorldName, Password: envelope,
			Public: body.Public, Crossplay: body.Crossplay, CrossplayInstanceID: id,
			Preset: body.Preset, Modifiers: modifiers, MemLimitMB: memLimitMB,
		})
		if err == nil {
			return basePort, nil
		}
		if !errors.Is(err, store.ErrBasePortTaken) {
			return 0, fmt.Errorf("create instance row: %w", err)
		}
	}
	return 0, fmt.Errorf("create instance row: %w", err)
}

func writeCreateInstanceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInstanceNameTaken):
		apierr.Write(w, r, apierr.New(apierr.NameTaken).With("field", "name"))
	case errors.Is(err, instance.ErrPortsExhausted), errors.Is(err, store.ErrBasePortTaken):
		apierr.Write(w, r, apierr.New(apierr.PortExhausted).Wrap(err))
	default:
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
	}
}

func addLaunchViolation(val *apierr.Validation, v instance.LaunchViolation) {
	switch v.Rule {
	case instance.RulePasswordTooShort:
		val.Add("password", apierr.FieldTooShort,
			fmt.Sprintf("Password must be at least %d characters.", instance.MinPasswordLength))
	case instance.RulePasswordInName:
		val.Add("password", apierr.FieldPasswordInName,
			"Password must not be a substring of the server or world name.")
	case instance.RuleWorldSameAsServer:
		val.Add("world_name", apierr.FieldSameAsServerName,
			"World name must not equal the server name.")
	}
}

func encodeModifiers(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode modifiers: %w", err)
	}
	return string(raw), nil
}

// provisionRun is what the provision job's Runner needs, carried as one value rather than
// closed-over individually so runProvision's signature does not grow with every new field.
type provisionRun struct {
	instanceID          string
	name                string
	basePort            int
	dataDir             string
	serverName          string
	worldName           string
	password            string
	public              bool
	crossplay           bool
	crossplayInstanceID string
	preset              string
	modifiers           string
	extraArgs           string
	memLimitMB          int
	cpuLimit            *float64
	startAfterProvision bool
	mods                []resolveRequest
	// requestedBy is the user id to attribute this run to, or "" for a run the panel
	// started on its own — 12 §9.2's resume after a crash has no user behind it.
	requestedBy string
}

// clonePollInterval is how often CloneWithProgress samples the destination's size during a
// full (non-reflink) copy. Two seconds matches jobs.progress_interval's own throttle — no
// point polling faster than the row that reports it is allowed to change.
const clonePollInterval = 2 * time.Second

// ProvisionCancelPolicy is 12 §8's declared boundary for `provision`: cancellable through
// every checkpoint up to, but not including, container creation — everything before it is
// discardable, and nothing before it is a world. Registered once at startup
// (cmd/valmind/main.go) against the same Engine that runs the job.
func ProvisionCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case "", "dirs_created", "build_cached", "cloned":
		return true, ""
	default:
		return false, "container_created"
	}
}

// runProvision is the provision job's Runner (12 §6): no transaction, ever (C1). Each phase
// is idempotent — EnsureBuildCached and CloneWithProgress both skip work already done — so
// a from-scratch re-run after a crash (WP-15) converges rather than duplicating work, and
// the checkpoint written after each phase is what a future resume will key off.
func (h *Instances) runProvision(run *provisionRun) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		if outcome, stop := h.provisionDirs(ctx, jh, run); stop {
			return outcome
		}
		if outcome, stop := h.provisionBuildCache(ctx, jh, run); stop {
			return outcome
		}
		if outcome, stop := h.provisionClone(ctx, jh, run); stop {
			return outcome
		}
		return h.provisionCreateContainer(ctx, jh, run)
	}
}

func (h *Instances) provisionDirs(ctx context.Context, jh *jobs.Handle, run *provisionRun) (jobs.Outcome, bool) {
	jh.Progress(ctx, 2, "creating directories")
	if err := instance.EnsureInstanceDirs(run.dataDir); err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("create instance directories: %w", err)), true
	}
	return provisionCheckpoint(ctx, jh, run.instanceID, "dirs_created")
}

func (h *Instances) provisionBuildCache(ctx context.Context, jh *jobs.Handle, run *provisionRun) (jobs.Outcome, bool) {
	jh.Progress(ctx, 10, "downloading game files")
	if err := instance.EnsureBuildCached(ctx, &instance.BuildCacheInput{
		Runtime:      h.Runtime,
		Image:        h.Cfg.Game.SteamCMDImage,
		HostCacheDir: instance.CacheDir(h.Cfg.Data.HostRoot),
		CacheDir:     instance.CacheDir(h.Cfg.Data.Root),
		BuildID:      provisionBuildID,
		// A retry that says nothing reads as a hang: the download is the longest phase of
		// the longest job in the panel, and Q31's failure lands in the first seconds of it.
		Report: func(attempt, of int, err error) {
			jh.Log(fmt.Sprintf("steamcmd attempt %d of %d failed (%v); retrying", attempt, of, err))
			jh.Progress(ctx, 10, fmt.Sprintf("retrying download (attempt %d of %d)", attempt+1, of))
		},
	}); err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("build cache: %w", err)), true
	}
	return provisionCheckpoint(ctx, jh, run.instanceID, "build_cached")
}

func (h *Instances) provisionClone(ctx context.Context, jh *jobs.Handle, run *provisionRun) (jobs.Outcome, bool) {
	var fsType string
	_, _ = h.DB.KVGet(ctx, "data_fs_type", &fsType) // "" (unknown) degrades to the safe, slow-path budget
	cloneStart, cloneEnd := instance.CloneProgressBudget(fsType)
	jh.Progress(ctx, cloneStart, "cloning game files")

	srcDir := instance.CacheDir(h.Cfg.Data.Root) + "/" + provisionBuildID
	dstDir := run.dataDir + "/server"
	err := instance.CloneWithProgress(ctx, srcDir, dstDir, clonePollInterval, func(pct int) {
		jh.Progress(ctx, cloneStart+(cloneEnd-cloneStart)*pct/100, "cloning game files")
	})
	if err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("clone game files: %w", err)), true
	}
	if err := instance.VerifyClonedOwnership(dstDir, instance.WantCloneUID); err != nil {
		return provisionFailed(run.instanceID, err), true
	}
	return provisionCheckpoint(ctx, jh, run.instanceID, "cloned")
}

func (h *Instances) provisionCreateContainer(ctx context.Context, jh *jobs.Handle, run *provisionRun) jobs.Outcome {
	jh.Progress(ctx, 90, "creating container")
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: run.instanceID, DataDir: run.dataDir, BasePort: run.basePort,
		ServerName: run.serverName, WorldName: run.worldName, Password: run.password,
		Public: run.public, Crossplay: run.crossplay, CrossplayInstanceID: run.crossplayInstanceID,
		Preset: run.preset, Modifiers: run.modifiers, ExtraArgs: run.extraArgs,
		MemLimitMB: run.memLimitMB, CPULimit: run.cpuLimit,
	}, h.Cfg.Game.Image, h.Cfg.Game.StopTimeout.Std())
	if err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("build container spec: %w", err))
	}
	containerID, err := h.Runtime.Create(ctx, spec)
	if err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("create container: %w", err))
	}
	// Past this checkpoint the job is no longer cancellable (ProvisionCancelPolicy): a
	// container now exists, so nothing after this point is discardable for free.
	if err := jh.Checkpoint(ctx, "container_created"); err != nil {
		return provisionFailed(run.instanceID, err)
	}

	jh.Progress(ctx, 100, "provisioned")
	return jobs.Outcome{
		Status: "succeeded",
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxFinishProvisioning(ctx, tx, run.instanceID,
				string(instance.StateProvisioning), string(instance.StateStopped),
				containerID, provisionBuildID); err != nil {
				return fmt.Errorf("finish provisioning instance %s: %w", run.instanceID, err)
			}
			return nil
		},
		AfterFinish: h.afterProvision(run, containerID),
	}
}

// afterProvision is what happens once provisioning has succeeded and the instance has
// reached `stopped`: the mods the wizard chose are installed, and then — 12 §2.2's own
// words for ADR-033 — "a start job if the wizard asked for one". It runs from
// jobs.Outcome.AfterFinish rather than inside the Runner because a job cannot claim its own
// lock key while still holding it.
//
// nil when the wizard asked for neither, so the common case adds no hook at all.
//
// `↯` Mods before start, and that ordering is the whole point of Q42. The wizard can start
// the server itself, and Valheim writes the world on that first boot — so a mod installed
// afterwards arrives after the thing it may have wanted to influence, and installing it
// means stopping the server the wizard just started. Doing it here costs one hook and makes
// "create a modded server" one screen instead of three.
func (h *Instances) afterProvision(run *provisionRun, containerID string) func(context.Context) {
	if !run.startAfterProvision && len(run.mods) == 0 {
		return nil
	}
	return func(ctx context.Context) {
		h.installThenStart(ctx, run, containerID, run.mods)
	}
}

// installThenStart submits the first outstanding mod install, with itself as the
// continuation for the rest, and starts the server when none are left.
//
// `↯` A chain of single-package jobs rather than one job that takes a list. mod_install
// resolves and places one requested package plus its closure, holds the instance lock while
// it does, and is recoverable on its own (12 §9.4) — so N packages is N of those, in
// sequence. Each link is a job an operator can see, cancel and retry, and a crash between
// two links leaves an instance that is `stopped` with some mods installed, which is a state
// the mod screen already renders and the operator can finish by hand. The alternative — one
// job with a list — would need its own checkpoint semantics for partial application, which
// is precisely what 12 §9.4 already solved once.
func (h *Instances) installThenStart(
	ctx context.Context, run *provisionRun, containerID string, remaining []resolveRequest,
) {
	inst, err := h.DB.InstanceByID(ctx, run.instanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "after provision: instance vanished",
			slog.String("instance_id", run.instanceID), slog.Any("error", err))
		return
	}

	if len(remaining) > 0 {
		if h.Mods == nil {
			// Nothing wired the mod engine in. Refusing to start is the honest answer: the
			// operator asked for a modded server and would otherwise get a vanilla world
			// generated under a name that promises mods.
			slog.ErrorContext(ctx, "after provision: mods requested but no mod engine is wired",
				slog.String("instance_id", run.instanceID))
			return
		}
		next := remaining[0]
		err := h.Mods.SubmitInstall(ctx, inst, next, run.requestedBy, func(ctx context.Context) {
			h.installThenStart(ctx, run, containerID, remaining[1:])
		})
		if err != nil {
			// The chain stops here and the server is not started — see runModInstallThen.
			// The instance is `stopped` with whatever landed before this, which the mod
			// screen shows and the operator can finish from.
			slog.WarnContext(ctx, "install mod after provision",
				slog.String("instance_id", run.instanceID),
				slog.String("full_name", next.FullName), slog.Any("error", err))
		}
		return
	}

	if !run.startAfterProvision {
		return
	}
	// A failure to start is logged and left: the provision itself succeeded, the instance is
	// `stopped` and startable, and failing a completed 1 GB download over the start that
	// followed it would be the wrong report.
	if _, err := h.submitStart(ctx, inst, containerID, run.requestedBy); err != nil {
		slog.WarnContext(ctx, "start after provision",
			slog.String("instance_id", run.instanceID), slog.Any("error", err))
	}
}

// provisionCheckpoint writes checkpoint and reports whether the runner must stop here:
// either the write itself failed, or a cancel was requested while still within
// ProvisionCancelPolicy's cancellable range.
func provisionCheckpoint(ctx context.Context, jh *jobs.Handle, instanceID, checkpoint string) (jobs.Outcome, bool) {
	if err := jh.Checkpoint(ctx, checkpoint); err != nil {
		return provisionFailed(instanceID, err), true
	}
	if jh.CancelRequested(ctx) {
		return jobs.Outcome{Status: "cancelled", OnFinish: provisionOnFinishError(instanceID)}, true
	}
	return jobs.Outcome{}, false
}

func provisionFailed(instanceID string, err error) jobs.Outcome {
	return jobs.Outcome{
		Status: "failed", ErrorCode: apierr.Internal.String(), Error: err.Error(),
		OnFinish: provisionOnFinishError(instanceID),
	}
}

// provisionOnFinishError is the failed and cancelled paths' shared OnFinish (12 §8: "a
// cancelled job runs the same cleanup path as a failed one"). Partial artefacts — the
// directories, the cache entry, a half-cloned server/ — are left in place; cleanup is an
// explicit delete job, never implicit here.
func provisionOnFinishError(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := store.TxUpdateInstanceState(
			ctx, tx, instanceID, string(instance.StateProvisioning), string(instance.StateError)); err != nil {
			return fmt.Errorf("park instance %s in error: %w", instanceID, err)
		}
		return nil
	}
}
