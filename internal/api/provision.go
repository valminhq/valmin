package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/source"
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
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}

	var body createInstanceRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	h.createInstance(w, r, u, &body, control.OperationCreate, nil)
}

// createInstance is everything POST /instances does once it holds a request: validation, the
// row, the definition operation and the provision job. A manifest import arrives here too
// (ADR-151) with imported, the config bytes and side tags the chain applies once its mods are
// in, which is the only difference between the two. A create passes nil.
func (h *Instances) createInstance(
	w http.ResponseWriter, r *http.Request, u *store.User,
	body *createInstanceRequest, opKind string, imported *control.OperationPlan,
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
	dataDir := instance.DataDir(h.Cfg.Data.Root, id)
	envelope, err := h.Keeper.Encrypt(
		crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(id),
		[]byte(body.Password),
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	basePort, err := h.createInstanceRow(r.Context(), id, dataDir, envelope, modifiers, memLimitMB, body)
	if err != nil {
		writeCreateInstanceError(w, r, err)
		return
	}

	plan := &control.OperationPlan{Mods: domainPackages(body.Mods), Start: body.StartAfterProvision}
	if imported != nil {
		plan.Configs, plan.Sides = imported.Configs, imported.Sides
	}
	if err := h.ctl.Operations.Create(r.Context(), id, opKind, u.ID, plan); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	origin := "new"
	if opKind == control.OperationImport {
		origin = "manifest"
	}
	job, err := h.submitProvision(r.Context(), &control.ProvisionRun{
		InstanceID: id, Name: body.Name, BasePort: basePort, DataDir: dataDir,
		ServerName: body.ServerName, WorldName: body.WorldName, Password: body.Password,
		Public: body.Public, Crossplay: body.Crossplay, CrossplayInstanceID: id,
		Preset: body.Preset, Modifiers: modifiers, ExtraArgs: body.ExtraArgs,
		MemLimitMB: memLimitMB, CPULimit: body.CPULimit,
		StartAfterProvision: body.StartAfterProvision, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.create",
			map[string]string{"name": body.Name, "source": origin}),
	}, instance.StateCreated)
	if err != nil {
		var conflict *store.JobConflict
		if errors.As(err, &conflict) {
			apierr.Write(w, r, apierr.New(errcode.JobInProgress).With("job_id", conflict.JobID))
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
			continue
		}
		// An unrecognised registry is rejected rather than ignored, for decodePackageRequest's
		// reason: falling through to the other one installs different bytes (B14).
		if name := strings.TrimSpace(m.Source); name != "" {
			if _, ok := source.ByName(name); !ok {
				val.Add(fmt.Sprintf("mods[%d].source", i), apierr.FieldInvalid,
					"source must name a configured mod registry.")
			}
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
		apierr.Write(w, r, apierr.New(errcode.Unavailable).
			Wrap(errors.New("this panel has no mod engine, so mods cannot be installed at create")))
		return false
	}
	fresh := &store.Instance{}
	for _, req := range mods {
		if err := h.Mods.CheckResolvable(r.Context(), fresh, domainPackage(req)); err != nil {
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
	ctx context.Context, run *control.ProvisionRun, from instance.State,
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts and the submission error
	return h.ctl.Provisioner.Submit(ctx, run, from)
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
			Preset: body.Preset, Modifiers: modifiers, ExtraArgs: body.ExtraArgs,
			MemLimitMB: memLimitMB, CPULimit: body.CPULimit,
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
		apierr.Write(w, r, apierr.New(errcode.NameTaken).With("field", "name"))
	case errors.Is(err, instance.ErrPortsExhausted), errors.Is(err, store.ErrBasePortTaken):
		apierr.Write(w, r, apierr.New(errcode.PortExhausted).Wrap(err))
	default:
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
