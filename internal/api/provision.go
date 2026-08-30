package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
}

// provisionPayload is the provision job's persisted payload (ADR-033):
// start_after_provision lives here, not as an instances column, because it is an
// instruction for this one run, not a durable fact about the instance. `↯` Not yet acted
// on — the runner below stores it and stops; the start job that would read it is WP-14's.
type provisionPayload struct {
	StartAfterProvision bool `json:"start_after_provision"`
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
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
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

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind:         jobs.KindProvision,
		LockKey:      jobs.InstanceLockKey(id),
		InstanceID:   &id,
		InstanceName: body.Name,
		RequestedBy:  u.ID,
		Payload:      provisionPayload{StartAfterProvision: body.StartAfterProvision},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateCreated), string(instance.StateProvisioning))
			if err != nil {
				return fmt.Errorf("claim provision for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s not in created state at claim", id)
			}
			return nil
		},
	}, h.runProvision(&provisionRun{
		instanceID: id, basePort: basePort, dataDir: dataDir,
		serverName: body.ServerName, worldName: body.WorldName, password: body.Password,
		public: body.Public, crossplay: body.Crossplay, crossplayInstanceID: id,
		preset: body.Preset, modifiers: modifiers, extraArgs: body.ExtraArgs,
		memLimitMB: memLimitMB, cpuLimit: body.CPULimit,
	}))
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
