package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// createInstanceRequest is POST /instances' body. Every field it accepts
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

// provisionPayload is the provision job's persisted payload (ADR-033). The rest of the
// definition the wizard asked for lives on the instance's operation row, which outlives this
// job and is what the remaining steps are driven from (Q52).
type provisionPayload struct {
	StartAfterProvision bool `json:"start_after_provision"`
}

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
	h.createInstance(w, r, u, &body, opKindCreate, nil)
}

// createInstance is everything POST /instances does once it holds a request: validation, the
// row, the definition operation and the provision job. A manifest import arrives here too
// (ADR-151) with the config bytes the chain applies once its mods are in, which is the only
// difference between the two.
func (h *Instances) createInstance(
	w http.ResponseWriter, r *http.Request, u *store.User,
	body *createInstanceRequest, opKind string, configs []manifestConfig,
) {
	var val apierr.Validation
	if body.Name == "" {
		val.Add("name", apierr.FieldRequired, "Name is required.")
	}
	memLimitMB := body.MemLimitMB
	if memLimitMB == 0 {
		memLimitMB = h.Cfg.Game.DefaultMemMB
	}
	for _, v := range instance.ValidateLaunch(body.ServerName, body.WorldName, body.Password) {
		addLaunchViolation(&val, v)
	}
	for _, v := range instance.ValidateResources(memLimitMB, body.CPULimit) {
		addResourceViolation(&val, v)
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

	id := store.NewID()
	dataDir := h.localDataDir(id)
	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(id),
		[]byte(body.Password),
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	basePort, err := h.createInstanceRow(r.Context(), id, dataDir, envelope, modifiers, memLimitMB, body)
	if err != nil {
		writeCreateInstanceError(w, r, err)
		return
	}

	plan := &opPlan{Mods: body.Mods, Configs: configs, Start: body.StartAfterProvision}
	if err := h.createOperation(r.Context(), id, opKind, u.ID, plan); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	job, err := h.submitProvision(r.Context(), &provisionRun{
		instanceID: id, name: body.Name, basePort: basePort, dataDir: dataDir,
		serverName: body.ServerName, worldName: body.WorldName, password: body.Password,
		public: body.Public, crossplay: body.Crossplay, crossplayInstanceID: id,
		preset: body.Preset, modifiers: modifiers, extraArgs: body.ExtraArgs,
		memLimitMB: memLimitMB, cpuLimit: body.CPULimit,
		startAfterProvision: body.StartAfterProvision, requestedBy: u.ID,
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

// modsAreInstallable is the create request's mod check. It runs before the instance row and the
// port allocation, so an unresolvable package is a 409 naming it rather than a job that fails
// after the game download (Q42). It resolves against an instance that does not exist yet, which
// has nothing installed, as the one about to be created does not either.
//
// Writes the response and reports false when the request cannot go ahead.
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
// `created` for POST /instances and `provisioning` for a resume of a run whose process died
// (12 §9.2), which the compare-and-swap accepts as a self-transition.
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
		Payload:      provisionPayload{StartAfterProvision: run.startAfterProvision},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			var ok bool
			var err error
			if from == instance.StateProvisioning {
				ok, err = holdStateTx(ctx, tx, id, from)
			} else {
				ok, err = setStateTx(ctx, tx, id, from, instance.StateProvisioning)
			}
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

// createInstanceRow allocates a port and inserts the row, retrying a few times on
// store.ErrBasePortTaken. The base port is the panel's own choice, so a collision is a race to
// retry rather than a validation failure to report.
func (h *Instances) createInstanceRow(
	ctx context.Context, id, dataDir, envelope, modifiers string, memLimitMB int, body *createInstanceRequest,
) (basePort int, err error) {
	allocator := instance.NewAllocator(h.DB, h.Runtime, h.Cfg.Ports.Base, h.Cfg.Ports.Stride)
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
			CPULimit: body.CPULimit,
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

func addResourceViolation(val *apierr.Validation, v instance.ResourceViolation) {
	switch v.Rule {
	case instance.RuleMemoryBelowMinimum:
		val.Add(v.Field, apierr.FieldOutOfRange,
			fmt.Sprintf("Memory must be at least %d MB.", instance.MinMemoryLimitMB))
	case instance.RuleCPUNonPositive:
		val.Add(v.Field, apierr.FieldOutOfRange, "CPU limit must be greater than 0.")
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
	buildID             string
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
	// requestedBy is the user id to attribute this run to, or "" for a run the panel
	// started on its own — 12 §9.2's resume after a crash has no user behind it.
	requestedBy string
}

// clonePollInterval is how often CloneWithProgress samples the destination's size during a
// full (non-reflink) copy. Two seconds matches jobs.progress_interval's own throttle — no
// point polling faster than the row that reports it is allowed to change.
const clonePollInterval = 2 * time.Second

// ProvisionCancelPolicy is 12 §8's declared boundary for `provision`: cancellable through every
// checkpoint up to, but not including, container creation. Registered once at startup against
// the same Engine that runs the job.
func ProvisionCancelPolicy(checkpoint string) (cancellable bool, phase string) {
	switch checkpoint {
	case "", "dirs_created", "build_cached", "cloned":
		return true, ""
	default:
		return false, "container_created"
	}
}

// runProvision is the provision job's Runner (12 §6), holding no transaction (C1). Every phase
// is idempotent, so a from-scratch re-run after a crash converges; the checkpoint written after
// each phase is what a resume keys off.
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
	id, err := instance.CachePublicBuild(ctx, &instance.BuildCacheInput{
		Runtime:      h.Runtime,
		Image:        h.Cfg.Game.SteamCMDImage,
		HostCacheDir: instance.CacheDir(h.Cfg.Data.HostRoot),
		CacheDir:     instance.CacheDir(h.Cfg.Data.Root),
		// A retry that says nothing reads as a hang: the download is the longest phase of
		// the longest job in the panel, and Q31's failure lands in the first seconds of it.
		Report: func(attempt, of int, err error) {
			jh.Log(fmt.Sprintf("steamcmd attempt %d of %d failed (%v); retrying", attempt, of, err))
			jh.Progress(ctx, 10, fmt.Sprintf("retrying download (attempt %d of %d)", attempt+1, of))
		},
	})
	if err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("build cache: %w", err)), true
	}
	run.buildID = id
	return provisionCheckpoint(ctx, jh, run.instanceID, "build_cached")
}

func (h *Instances) provisionClone(ctx context.Context, jh *jobs.Handle, run *provisionRun) (jobs.Outcome, bool) {
	var fsType string
	_, _ = h.DB.KVGet(ctx, "data_fs_type", &fsType) // "" (unknown) degrades to the safe, slow-path budget
	cloneStart, cloneEnd := instance.CloneProgressBudget(fsType)
	jh.Progress(ctx, cloneStart, "cloning game files")

	srcDir := instance.CacheDir(h.Cfg.Data.Root) + "/" + run.buildID
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
	run.buildID, err = instance.InstalledBuildID(run.dataDir)
	if err != nil {
		return provisionFailed(run.instanceID, err), true
	}
	return provisionCheckpoint(ctx, jh, run.instanceID, "cloned")
}

func (h *Instances) provisionCreateContainer(ctx context.Context, jh *jobs.Handle, run *provisionRun) jobs.Outcome {
	jh.Progress(ctx, 90, "creating container")
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: run.instanceID, DataDir: h.hostDataDir(run.instanceID), BasePort: run.basePort,
		ServerName: run.serverName, WorldName: run.worldName, Password: run.password,
		Public: run.public, Crossplay: run.crossplay, CrossplayInstanceID: run.crossplayInstanceID,
		Preset: run.preset, Modifiers: run.modifiers, ExtraArgs: run.extraArgs,
		MemLimitMB: run.memLimitMB, CPULimit: run.cpuLimit,
	}, h.Cfg.Game.Image, h.Cfg.Game.Network, h.Cfg.Game.StopTimeout.Std())
	if err != nil {
		return provisionFailed(run.instanceID, fmt.Errorf("build container spec: %w", err))
	}
	containerID, err := h.ensureInstanceContainer(ctx, spec)
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
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			if err := finishProvisioningState(ctx, tx, run.instanceID,
				instance.StateProvisioning, instance.StateStopped,
				containerID, run.buildID); err != nil {
				return fmt.Errorf("finish provisioning instance %s: %w", run.instanceID, err)
			}
			return nil
		},
		AfterFinish: func(ctx context.Context) { h.advanceChain(ctx, run.instanceID) },
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
		return jobs.Outcome{Status: jobs.StatusCancelled, OnFinish: provisionOnFinishError(instanceID)}, true
	}
	return jobs.Outcome{}, false
}

func provisionFailed(instanceID string, err error) jobs.Outcome {
	return jobs.Outcome{
		Status: jobs.StatusFailed, ErrorCode: apierr.Internal.String(), Error: err.Error(),
		OnFinish: provisionOnFinishError(instanceID),
	}
}

// provisionOnFinishError is the failed and cancelled paths' shared OnFinish (12 §8). Partial
// artefacts are left in place: the directories, the cache entry and a half-cloned server/ are
// removed by an explicit delete job, never implicitly here.
func provisionOnFinishError(instanceID string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := setStateTx(
			ctx, tx, instanceID, instance.StateProvisioning, instance.StateError); err != nil {
			return fmt.Errorf("park instance %s in error: %w", instanceID, err)
		}
		return nil
	}
}
