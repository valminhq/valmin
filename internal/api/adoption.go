package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

type adoptionRequest struct {
	Name       string             `json:"name"`
	ServerName string             `json:"server_name"`
	WorldName  string             `json:"world_name"`
	Password   string             `json:"password"`
	Public     *bool              `json:"public"`
	Crossplay  *bool              `json:"crossplay"`
	Preset     *string            `json:"preset"`
	Modifiers  *map[string]string `json:"modifiers"`
	ExtraArgs  *string            `json:"extra_args"`
	MemLimitMB *int               `json:"mem_limit_mb"`
	CPULimit   optionalFloat64    `json:"cpu_limit"`
}

type adoptionPreview struct {
	Orphan
	CrossplayInstanceID string   `json:"crossplay_instance_id"`
	GameBuildID         string   `json:"game_build_id"`
	Modded              bool     `json:"modded"`
	Required            []string `json:"required_fields"`
}

type adoptionFacts struct {
	container    *runtime.Container
	instanceID   string
	crossplayID  string
	basePort     int
	localDataDir string
	hostDataDir  string
	gameBuildID  string
	modded       bool
	state        string
}

type adoptionPayload struct {
	ContainerID string `json:"container_id"`
}

var adoptionRequiredFields = []string{
	"name", "server_name", "world_name", "password", "public", "crossplay", "preset",
	"modifiers", "extra_args", "mem_limit_mb", "cpu_limit",
}

// previewAdoption reports the non-secret facts the daemon can prove about an orphan. The
// mutable launch definition is deliberately absent because the admin must confirm it.
func (h *Instances) previewAdoption(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	facts, err := h.adoptionFacts(r.Context(), r.PathValue("container_id"), "")
	if err != nil {
		writeAdoptionError(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, adoptionPreview{
		Orphan: Orphan{
			ContainerID: facts.container.ID, Name: facts.container.Name,
			InstanceID: facts.instanceID, BasePort: facts.basePort, Running: facts.container.Running,
		},
		CrossplayInstanceID: facts.crossplayID,
		GameBuildID:         facts.gameBuildID, Modded: facts.modded,
		Required: append([]string(nil), adoptionRequiredFields...),
	})
}

// adopt reconstructs one row from a managed orphan without changing the container or its
// bind-mounted files. All external checks finish before the job's claim transaction begins.
func (h *Instances) adopt(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	var body adoptionRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	modifiers, err := validateAdoptionRequest(&body)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	facts, err := h.adoptionFacts(r.Context(), r.PathValue("container_id"), body.WorldName)
	if err != nil {
		writeAdoptionError(w, r, err)
		return
	}
	launch := &instance.LaunchSpec{
		InstanceID: facts.instanceID, DataDir: facts.hostDataDir, BasePort: facts.basePort,
		ServerName: body.ServerName, WorldName: body.WorldName, Password: body.Password,
		Public: *body.Public, Crossplay: *body.Crossplay, CrossplayInstanceID: facts.crossplayID,
		Preset: *body.Preset, Modifiers: modifiers, ExtraArgs: *body.ExtraArgs,
		MemLimitMB: *body.MemLimitMB, CPULimit: body.CPULimit.value,
	}
	if err := instance.ValidateAdoptionLaunch(
		facts.container, launch, h.Cfg.Game.Image, h.Cfg.Game.Network, h.Cfg.Game.StopTimeout.Std(),
	); err != nil {
		writeAdoptionError(w, r, err)
		return
	}

	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(facts.instanceID),
		[]byte(body.Password),
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	job, err := h.submitAdoption(r.Context(), u.ID, middleware.ClientIPFrom(r.Context()).String(),
		facts, &body, modifiers, envelope)
	if err != nil {
		writeAdoptionError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

func validateAdoptionRequest(body *adoptionRequest) (string, error) {
	var val apierr.Validation
	for _, field := range []struct {
		name  string
		value string
	}{
		{"name", body.Name},
		{"server_name", body.ServerName},
		{"world_name", body.WorldName},
		{"password", body.Password},
	} {
		if strings.TrimSpace(field.value) == "" {
			val.Add(field.name, apierr.FieldRequired, "This field is required.")
		}
	}
	if strings.ContainsAny(body.WorldName, `/\`) {
		val.Add("world_name", apierr.FieldInvalid, "World name must be a single safe file name.")
	}
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"public", body.Public != nil},
		{"crossplay", body.Crossplay != nil},
		{"preset", body.Preset != nil},
		{"modifiers", body.Modifiers != nil},
		{"extra_args", body.ExtraArgs != nil},
		{"mem_limit_mb", body.MemLimitMB != nil},
		{"cpu_limit", body.CPULimit.set},
	} {
		if !field.set {
			val.Add(field.name, apierr.FieldRequired, "This field is required.")
		}
	}
	for _, violation := range instance.ValidateLaunch(body.ServerName, body.WorldName, body.Password) {
		addLaunchViolation(&val, violation)
	}
	if body.MemLimitMB != nil {
		for _, violation := range instance.ValidateResources(*body.MemLimitMB, body.CPULimit.value) {
			addResourceViolation(&val, violation)
		}
	}
	modifiers := ""
	if body.Modifiers != nil {
		var err error
		modifiers, err = encodeModifiers(*body.Modifiers)
		if err != nil {
			val.Add("modifiers", apierr.FieldInvalid, "Modifiers must be a flat object of strings.")
		}
	}
	if err := val.Err(); err != nil {
		return "", fmt.Errorf("validate adoption request: %w", err)
	}
	return modifiers, nil
}

func (h *Instances) adoptionFacts(
	ctx context.Context, containerID, worldName string,
) (*adoptionFacts, error) {
	container, err := h.Runtime.Inspect(ctx, containerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil, apierr.New(apierr.NotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect adoption candidate: %w", err)
	}
	crossplayID, err := instance.ValidateAdoptionContainer(&container)
	if err != nil {
		return nil, fmt.Errorf("validate adoption candidate: %w", err)
	}
	instanceID := container.Labels[instance.LabelInstanceID]
	basePort, err := strconv.Atoi(container.Labels[instance.LabelBasePort])
	if err != nil {
		return nil, fmt.Errorf("read adoption base port: %w", err)
	}
	if err := h.ensureAdoptionUnclaimed(ctx, instanceID, container.ID); err != nil {
		return nil, err
	}
	disk, err := h.adoptionDiskFacts(instanceID, worldName)
	if err != nil {
		return nil, err
	}
	state := string(instance.StateStopped)
	if container.Running {
		state = string(instance.StateRunning)
	}
	return &adoptionFacts{
		container: &container, instanceID: instanceID, crossplayID: crossplayID, basePort: basePort,
		localDataDir: disk.localDataDir, hostDataDir: disk.hostDataDir, gameBuildID: disk.gameBuildID,
		modded: disk.modded, state: state,
	}, nil
}

func (h *Instances) ensureAdoptionUnclaimed(ctx context.Context, instanceID, containerID string) error {
	claimed, err := h.DB.InstanceByID(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("look up adoption instance identity: %w", err)
	}
	if claimed != nil {
		return store.ErrInstanceIDTaken
	}
	claimed, err = h.DB.InstanceByContainerID(ctx, containerID)
	if err != nil {
		return fmt.Errorf("look up adoption container owner: %w", err)
	}
	if claimed != nil {
		return store.ErrInstanceIDTaken
	}
	return nil
}

type adoptionDiskFacts struct {
	localDataDir string
	hostDataDir  string
	gameBuildID  string
	modded       bool
}

func (h *Instances) adoptionDiskFacts(instanceID, worldName string) (*adoptionDiskFacts, error) {
	localDataDir := filepath.Join(h.Cfg.Data.Root, "instances", instanceID)
	rootInfo, err := os.Lstat(localDataDir)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("adoption instance directory is missing or invalid: %w",
			instance.ErrContainerMismatch)
	}
	for _, dir := range []string{"server", "worlds", "logs"} {
		info, err := os.Lstat(filepath.Join(localDataDir, dir))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("adoption %s directory is missing or invalid: %w",
				dir, instance.ErrContainerMismatch)
		}
	}
	buildID, err := instance.InstalledBuildID(localDataDir)
	if err != nil {
		return nil, fmt.Errorf("read adopted game build: %w: %w", err, instance.ErrContainerMismatch)
	}
	if err := validateAdoptionWorldPair(localDataDir, worldName); err != nil {
		return nil, err
	}
	_, modErr := os.Stat(filepath.Join(instance.ServerDir(localDataDir),
		"doorstop_libs", "libdoorstop_x64.so"))
	if modErr != nil && !errors.Is(modErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect adopted mod loader: %w", modErr)
	}
	return &adoptionDiskFacts{
		localDataDir: localDataDir,
		hostDataDir:  filepath.Join(h.Cfg.Data.HostRoot, "instances", instanceID),
		gameBuildID:  buildID, modded: modErr == nil,
	}, nil
}

func validateAdoptionWorldPair(dataDir, worldName string) error {
	if worldName == "" {
		return nil
	}
	dir := filepath.Join(instance.WorldsDir(dataDir), instance.WorldsLocalDir)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("adopted world directory is missing or invalid: %w",
			instance.ErrContainerMismatch)
	}
	// Either of 03 §4's layouts, since an adopted container was provisioned by something else
	// and may be running any build (ADR-179).
	scan, err := backup.ScanWorlds(instance.WorldsDir(dataDir))
	if err != nil {
		return fmt.Errorf("inspect adopted world: %w", err)
	}
	world, present := scan[worldName]
	if !present {
		return fmt.Errorf("adopted world is missing: %w", instance.ErrContainerMismatch)
	}
	if !world.Complete() {
		return fmt.Errorf("adopted world is missing half of itself: %w",
			instance.ErrContainerMismatch)
	}
	return nil
}

func (h *Instances) submitAdoption(
	ctx context.Context, requestedBy, auditIP string, facts *adoptionFacts,
	body *adoptionRequest, modifiers, password string,
) (*store.Job, error) {
	instanceID := facts.instanceID
	detail, err := json.Marshal(map[string]any{
		"container_id": facts.container.ID, "instance_id": instanceID,
		"state": facts.state, "game_build_id": facts.gameBuildID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode adoption audit detail: %w", err)
	}
	preset, extraArgs := stringPointer(*body.Preset), stringPointer(*body.ExtraArgs)
	var modifiersPtr *string
	if modifiers != "" {
		modifiersPtr = &modifiers
	}
	job, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindAdopt, LockKey: jobs.InstanceLockKey(instanceID),
		InstanceID: &instanceID, InstanceName: body.Name, RequestedBy: requestedBy,
		Payload: adoptionPayload{ContainerID: facts.container.ID},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxAdoptInstance(ctx, tx, &store.AdoptedInstance{
				ID: instanceID, Name: body.Name, State: facts.state, ContainerID: facts.container.ID,
				DataDir: facts.localDataDir, BasePort: facts.basePort,
				ServerName: body.ServerName, WorldName: body.WorldName, Password: password,
				Public: *body.Public, Crossplay: *body.Crossplay, CrossplayInstanceID: facts.crossplayID,
				Preset: preset, Modifiers: modifiersPtr, ExtraArgs: extraArgs,
				Modded: facts.modded, MemLimitMB: *body.MemLimitMB,
				CPULimit: body.CPULimit.value, GameBuildID: facts.gameBuildID,
			}); err != nil {
				return fmt.Errorf("publish adopted instance: %w", err)
			}
			if err := store.TxWriteAuditLog(ctx, tx, &store.AuditEntry{
				UserID: requestedBy, InstanceID: instanceID, Action: authz.InstanceAdopt.String(),
				Detail: string(detail), IP: auditIP,
			}); err != nil {
				return fmt.Errorf("write adoption audit: %w", err)
			}
			return nil
		},
	}, func(ctx context.Context, handle *jobs.Handle) jobs.Outcome {
		handle.Progress(ctx, 100, "adoption complete")
		return jobs.Outcome{Status: jobs.StatusSucceeded}
	})
	if err != nil {
		return nil, fmt.Errorf("submit adoption job: %w", err)
	}
	return job, nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func writeAdoptionError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.JobConflict
	switch {
	case errors.As(err, &conflict):
		writeJobSubmitError(w, r, err)
	case errors.Is(err, instance.ErrContainerMismatch):
		apierr.Write(w, r, apierr.New(apierr.ContainerMismatch).Wrap(err))
	case errors.Is(err, store.ErrInstanceNameTaken):
		apierr.Write(w, r, apierr.New(apierr.NameTaken))
	case errors.Is(err, store.ErrBasePortTaken), errors.Is(err, store.ErrInstanceIDTaken):
		apierr.Write(w, r, apierr.New(apierr.InvalidState).With("state", "claimed"))
	default:
		apierr.Write(w, r, err)
	}
}
