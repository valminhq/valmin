package api

import (
	"context"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Instances serves the instance surface: creation (a job), the read-side CRUD, the limited
// PATCH, the audited password endpoint, the one way out of `error`, and the lifecycle jobs
// — start, stop, restart and delete, in lifecycle.go.
type Instances struct {
	DB      *store.DB
	Authz   *authz.Authz
	Runtime runtime.Runtime
	Keeper  *crypto.Keeper
	Engine  *jobs.Engine
	Cfg     *config.Config
	// Streams holds one log reader and one stats sampler per running instance, plus the ring
	// buffer each reader fills (14 §1). It is the hub's source for the console and stats
	// topics and jobs' source for matched lines; the handlers themselves never read from it.
	Streams *instance.Streams

	// Mods is the create wizard's hook into the mod engine (Q42). Nil in a panel with no
	// mod engine, in which case create refuses a request that names mods rather than
	// quietly provisioning a vanilla server under a name that promised otherwise.
	Mods ModEngine
}

// ModEngine is the slice of the mod engine the create path needs — declared here, by the
// consumer, per 06 §4. Mods satisfies it and is constructed after this struct, so it is
// wired in router.go rather than made a construction-order problem.
type ModEngine interface {
	// CheckResolvable reports whether one requested package's whole closure can be computed
	// from the cached index, without writing or downloading anything. inst may describe an
	// instance that does not exist yet: nothing is installed on it, so the answer is the
	// closure a fresh server would get.
	CheckResolvable(ctx context.Context, inst *store.Instance, req resolveRequest) error

	// SubmitInstall queues one mod_install job, running afterFinish only if it succeeds.
	SubmitInstall(
		ctx context.Context,
		inst *store.Instance,
		req resolveRequest,
		requestedBy string,
		afterFinish func(context.Context),
	) error
}

func (h *Instances) Routes(rt *Router) {
	rt.Handle("GET /api/v1/instances", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/instances", http.HandlerFunc(h.create))
	// Ahead of /instances/{id}: ServeMux prefers the literal segment, so "orphans" cannot
	// be read as an id, but registering it first keeps that obvious to a reader too.
	rt.Handle("GET /api/v1/instances/orphans", http.HandlerFunc(h.orphans))
	rt.Handle("GET /api/v1/game/options", http.HandlerFunc(h.options))
	rt.Handle("GET /api/v1/instances/{id}", http.HandlerFunc(h.get))
	rt.Handle("PATCH /api/v1/instances/{id}", http.HandlerFunc(h.patch))
	rt.Handle("GET /api/v1/instances/{id}/password", http.HandlerFunc(h.password))
	rt.Handle("GET /api/v1/instances/{id}/logs", http.HandlerFunc(h.logs))
	rt.Handle("GET /api/v1/instances/{id}/stats", http.HandlerFunc(h.stats))
	rt.Handle("GET /api/v1/instances/{id}/jobs", http.HandlerFunc(h.jobHistory))
	rt.Handle("GET /api/v1/instances/{id}/disk", http.HandlerFunc(h.disk))
	rt.Handle("POST /api/v1/instances/{id}/acknowledge", http.HandlerFunc(h.acknowledge))
	rt.Handle("POST /api/v1/instances/{id}/start", http.HandlerFunc(h.start))
	rt.Handle("POST /api/v1/instances/{id}/stop", http.HandlerFunc(h.stop))
	rt.Handle("POST /api/v1/instances/{id}/restart", http.HandlerFunc(h.restart))
	rt.Handle("DELETE /api/v1/instances/{id}", http.HandlerFunc(h.delete))
	h.listRoutes(rt)
	h.configRoutes(rt)
	// Stream, not Handle: 11 §8.1's 30 s TimeoutHandler would sever a multi-hundred-
	// megabyte upload mid-transfer, and the client would see a timeout it cannot act on.
	rt.Stream("POST /api/v1/instances/{id}/worlds/import", http.HandlerFunc(h.importWorld))
}

// instanceView is the row plus what only a running container knows.
type instanceView struct {
	*store.Instance
	// CrossplayJoinCode is this boot's code, null until the session logs one (Q25). It is
	// read from the log rather than stored: a code from a previous boot is not this
	// server's, and a stale one sends a friend to a session that no longer exists.
	CrossplayJoinCode *string `json:"crossplay_join_code"`
}

func (h *Instances) view(inst *store.Instance) instanceView {
	v := instanceView{Instance: inst}
	if reader := h.Streams.Reader(inst.ID); reader != nil {
		if code := reader.JoinCode(); code != "" {
			v.CrossplayJoinCode = &code
		}
	}
	return v
}

// list is GET /instances: every instance for admin, grant-scoped for a member (09 §1).
func (h *Instances) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	ids, all, err := h.Authz.VisibleInstances(r.Context(), u)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if all {
		ids = nil // ListInstances(nil) is "every row"; VisibleInstances(all=true) carries none
	}
	instances, err := h.DB.ListInstances(r.Context(), ids)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	views := make([]instanceView, len(instances))
	for i := range instances {
		views[i] = h.view(&instances[i])
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

// get is GET /instances/{id}. An instance the caller cannot see does not exist (D2,
// ADR-038) — the same 404 for "no such id" and "not yours to see".
func (h *Instances) get(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	JSON(w, r, http.StatusOK, h.view(inst))
}

type patchInstanceRequest struct {
	ServerName *string            `json:"server_name"`
	Password   *string            `json:"password"`
	Public     *bool              `json:"public"`
	Crossplay  *bool              `json:"crossplay"`
	Preset     *string            `json:"preset"`
	Modifiers  *map[string]string `json:"modifiers"`
	MemLimitMB *int               `json:"mem_limit_mb"`
	CPULimit   *float64           `json:"cpu_limit"`
	ExtraArgs  *string            `json:"extra_args"`
}

// actions lists the capabilities this body's fields require. The mapping is data so that a
// field cannot be added without choosing one; the Can() calls themselves stay at the
// handler's own call site, where ADR-037 requires them to be visible.
//
// world_name is absent from the struct on purpose: -world names the save file basename
// (03 §1.3, ADR-077), so renaming it moves the .db and .fwl pair and needs 03 §4.1's
// handling rather than a column write. ADR-050's unknown-field decoding turns it into a 422
// naming the field, which tells an operator it is unsupported rather than dropping it
// silently (Q48).
func (b *patchInstanceRequest) actions() []authz.Action {
	var need []authz.Action
	if b.MemLimitMB != nil || b.CPULimit != nil {
		need = append(need, authz.InstanceLimits)
	}
	if b.ExtraArgs != nil {
		need = append(need, authz.InstanceExtraArgs)
	}
	if b.ServerName != nil || b.Password != nil || b.Public != nil ||
		b.Crossplay != nil || b.Preset != nil || b.Modifiers != nil {
		need = append(need, authz.InstanceSettings)
	}
	return need
}

// mergeInstanceLaunch is PATCH semantics (11 §1.1): absent means unchanged, so every field
// starts from current and only what body actually set overrides it. password carries the
// caller's already-encrypted envelope, since current holds one too.
func mergeInstanceLaunch(current *store.Instance, body patchInstanceRequest, password string) store.InstanceLaunch {
	patch := store.InstanceLaunch{
		ServerName: current.ServerName,
		Password:   password,
		Public:     current.Public,
		Crossplay:  current.Crossplay,
		Preset:     current.Preset,
		Modifiers:  current.Modifiers,
		MemLimitMB: current.MemLimitMB,
		CPULimit:   current.CPULimit,
		ExtraArgs:  current.ExtraArgs,
	}
	if body.ServerName != nil {
		patch.ServerName = *body.ServerName
	}
	if body.Public != nil {
		patch.Public = *body.Public
	}
	if body.Crossplay != nil {
		patch.Crossplay = *body.Crossplay
	}
	if body.Preset != nil {
		patch.Preset = body.Preset
	}
	if body.MemLimitMB != nil {
		patch.MemLimitMB = *body.MemLimitMB
	}
	if body.CPULimit != nil {
		patch.CPULimit = body.CPULimit
	}
	if body.ExtraArgs != nil {
		patch.ExtraArgs = body.ExtraArgs
	}
	return patch
}

// mergePatch validates the body against the row it is being applied to and produces the
// update. The three 03 §1.3 rules are checked on the merged result, not on the body: a
// password that is fine on its own can still be a substring of a server name the caller
// never mentioned. 08 §5.1 checks the same three again at container creation (G2).
func (h *Instances) mergePatch(
	w http.ResponseWriter, r *http.Request, current *store.Instance, body patchInstanceRequest,
) (store.InstanceLaunch, bool) {
	var val apierr.Validation

	serverName := current.ServerName
	if body.ServerName != nil {
		serverName = *body.ServerName
	}
	password, ok := h.patchPassword(w, r, current, body, serverName, &val)
	if !ok {
		return store.InstanceLaunch{}, false
	}

	patch := mergeInstanceLaunch(current, body, password)
	if body.Modifiers != nil {
		encoded, err := encodeModifiers(*body.Modifiers)
		if err != nil {
			val.Add("modifiers", apierr.FieldInvalid, "Modifiers must be a flat object of strings.")
		}
		patch.Modifiers = &encoded
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return store.InstanceLaunch{}, false
	}
	return patch, true
}

// patchPassword resolves the password the merged row should carry. An unchanged password is
// decrypted only to validate against it — 03 §1.3 rule 2 forbids a password that is a
// substring of the server name, so renaming the server can break a password the caller never
// mentioned — and its stored envelope is then written back untouched.
func (h *Instances) patchPassword(
	w http.ResponseWriter, r *http.Request, current *store.Instance,
	body patchInstanceRequest, serverName string, val *apierr.Validation,
) (envelope string, ok bool) {
	stored, err := h.DB.InstancePassword(r.Context(), current.ID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return "", false
	}
	location := crypto.Location{Table: "instances", Column: "password", RowID: current.ID}
	plaintext, err := h.Keeper.Decrypt(crypto.PurposeInstancePassword, location, stored)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return "", false
	}
	envelope = stored
	if body.Password != nil {
		plaintext = []byte(*body.Password)
	}

	for _, v := range instance.ValidateLaunch(serverName, current.WorldName, string(plaintext)) {
		addLaunchViolation(val, v)
	}
	if val.Err() != nil || body.Password == nil {
		return envelope, true
	}
	if envelope, err = h.Keeper.Encrypt(crypto.PurposeInstancePassword, location, plaintext); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return "", false
	}
	return envelope, true
}

// patch handles PATCH /instances/{id}, the launch config. Three capabilities gate it by
// field: instance.limits for the resource limits, instance.extra_args for the argv tail, and
// instance.settings for the rest. A change takes effect on the next start, which rebuilds
// the container when the row no longer describes it (ADR-118).
func (h *Instances) patch(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	var body patchInstanceRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	for _, action := range body.actions() {
		if !h.Authz.Can(r.Context(), u, action, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
	}

	current, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if current == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}

	patch, ok := h.mergePatch(w, r, current, body)
	if !ok {
		return
	}
	if err := h.DB.UpdateInstanceLaunch(r.Context(), id, &patch); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}

type instancePassword struct {
	Password string `json:"password"`
}

// password is GET /instances/{id}/password (11 §9): a live game secret, not a credential
// to verify, readable by any caller with instance.view but kept out of every other
// payload — and every read of this one writes an audit_log row.
func (h *Instances) password(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	exists, err := h.DB.InstanceExists(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if !exists {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	envelope, err := h.DB.InstancePassword(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	plaintext, err := h.Keeper.Decrypt(
		crypto.PurposeInstancePassword, crypto.Location{Table: "instances", Column: "password", RowID: id}, envelope,
	)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.password.read",
		IP: middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, instancePassword{Password: string(plaintext)})
}

// acknowledge is POST /instances/{id}/acknowledge (12 §2.4): the only way out of `error`.
// It re-runs the observer's own reconciliation question for this one instance and lands
// where reality supports — deliberately not "clear the flag" — rather than trusting that
// whatever caused the error has been fixed.
func (h *Instances) acknowledge(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if instance.State(inst.State) != instance.StateError {
		apierr.Write(w, r, apierr.New(apierr.InvalidState).
			With("state", inst.State).
			With("allowed_states", []instance.State{instance.StateError}))
		return
	}

	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}
	next, err := instance.Reconcile(r.Context(), h.Runtime, containerID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	if _, err := h.DB.UpdateInstanceState(r.Context(), id, inst.State, string(next)); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}
