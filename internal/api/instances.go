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

// Instances serves the instance surface: creation, the read-side CRUD, the launch-config
// PATCH, the audited password endpoint, acknowledge, and the lifecycle jobs in lifecycle.go.
type Instances struct {
	DB      *store.DB
	Authz   *authz.Authz
	Runtime runtime.Runtime
	Keeper  *crypto.Keeper
	Engine  *jobs.Engine
	Cfg     *config.Config
	// Streams holds one log reader and one stats sampler per running instance, plus the ring
	// buffer each reader fills (14 §1). It is the source for the console and stats topics and
	// for jobs waiting on a matched line.
	Streams *instance.Streams

	// Mods is the create wizard's hook into the mod engine (Q42). Nil in a panel with no mod
	// engine, where create refuses a request that names mods rather than provisioning a
	// vanilla server.
	Mods ModEngine
}

// ModEngine is the slice of the mod engine the create path needs, declared by the consumer
// (06 §4). Mods satisfies it and is wired in router.go.
type ModEngine interface {
	// CheckResolvable reports whether one requested package's whole closure can be computed
	// from the cached index, writing and downloading nothing. inst may describe an instance
	// that does not exist yet, in which case the answer is a fresh server's closure.
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
	// Registered ahead of /instances/{id}, which ServeMux would resolve the same way.
	rt.Handle("GET /api/v1/instances/orphans", http.HandlerFunc(h.orphans))
	rt.Handle("GET /api/v1/game/options", http.HandlerFunc(h.options))
	rt.Handle("GET /api/v1/instances/{id}", http.HandlerFunc(h.get))
	rt.Handle("PATCH /api/v1/instances/{id}", http.HandlerFunc(h.patch))
	rt.Handle("GET /api/v1/instances/{id}/password", http.HandlerFunc(h.password))
	rt.Handle("GET /api/v1/instances/{id}/logs", http.HandlerFunc(h.logs))
	rt.Handle("GET /api/v1/instances/{id}/stats", http.HandlerFunc(h.stats))
	rt.Handle("GET /api/v1/instances/{id}/jobs", http.HandlerFunc(h.jobHistory))
	rt.Handle("GET /api/v1/instances/{id}/disk", http.HandlerFunc(h.disk))
	rt.Handle("GET /api/v1/instances/{id}/backups", http.HandlerFunc(h.listBackups))
	rt.Handle("POST /api/v1/instances/{id}/backups", http.HandlerFunc(h.createBackup))
	rt.Handle("DELETE /api/v1/instances/{id}/backups/{bid}", http.HandlerFunc(h.deleteBackup))
	rt.Handle("POST /api/v1/instances/{id}/acknowledge", http.HandlerFunc(h.acknowledge))
	rt.Handle("POST /api/v1/instances/{id}/start", http.HandlerFunc(h.start))
	rt.Handle("POST /api/v1/instances/{id}/stop", http.HandlerFunc(h.stop))
	rt.Handle("POST /api/v1/instances/{id}/restart", http.HandlerFunc(h.restart))
	rt.Handle("DELETE /api/v1/instances/{id}", http.HandlerFunc(h.delete))
	h.listRoutes(rt)
	h.configRoutes(rt)
	// Stream, not Handle: 11 §8.1's 30 s TimeoutHandler would sever a large upload
	// mid-transfer.
	rt.Stream("POST /api/v1/instances/{id}/worlds/import", http.HandlerFunc(h.importWorld))
	// Stream for the same reason in the other direction: a world archive over a slow link
	// outlasts the request timeout, and a severed download is a corrupt file the operator
	// only discovers when they try to restore from it.
	rt.Stream("GET /api/v1/instances/{id}/backups/{bid}/download", http.HandlerFunc(h.downloadBackup))
}

// instanceView is the row plus what only a running container knows.
type instanceView struct {
	*store.Instance
	// CrossplayJoinCode is this boot's code, null until the session logs one (Q25). Read from
	// the log rather than stored, since a previous boot's code names a session that is gone.
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

// get is GET /instances/{id}. An instance the caller cannot see returns the same 404 as one
// that does not exist (D2, ADR-038).
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
	// Backup retention and the restart archive. Not launch fields: they shape no container,
	// so changing one sets no restart_required (ADR-118's drift check would not see it
	// either).
	BackupKeepCold  *int  `json:"backup_keep_cold"`
	BackupKeepHot   *int  `json:"backup_keep_hot"`
	BackupOnRestart *bool `json:"backup_on_restart"`
}

// backupPolicy reports whether the body touches retention at all.
func (b *patchInstanceRequest) backupPolicy() bool {
	return b.BackupKeepCold != nil || b.BackupKeepHot != nil || b.BackupOnRestart != nil
}

// actions lists the capabilities this body's fields require. Data, so a field cannot be added
// without choosing one; the Can() calls stay at the handler's call site (ADR-037).
//
// world_name is absent from the struct on purpose: -world names the save file basename, so
// renaming it moves the .db and .fwl pair and needs 03 §4.1's handling rather than a column
// write. Unknown-field decoding turns it into a 422 naming the field (ADR-050, Q48).
func (b *patchInstanceRequest) actions() []authz.Action {
	var need []authz.Action
	if b.MemLimitMB != nil || b.CPULimit != nil {
		need = append(need, authz.InstanceLimits)
	}
	if b.ExtraArgs != nil {
		need = append(need, authz.InstanceExtraArgs)
	}
	if b.ServerName != nil || b.Password != nil || b.Public != nil ||
		b.Crossplay != nil || b.Preset != nil || b.Modifiers != nil || b.backupPolicy() {
		need = append(need, authz.InstanceSettings)
	}
	return need
}

// mergeInstanceLaunch is PATCH semantics (11 §1.1): every field starts from current and only
// what body set overrides it. password carries an already-encrypted envelope, as current does.
func mergeInstanceLaunch(current *store.Instance, body *patchInstanceRequest, password string) store.InstanceLaunch {
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

// mergeBackupPolicy is PATCH semantics for the retention fields: each starts from the row and
// only what body set overrides it.
func mergeBackupPolicy(current *store.Instance, body *patchInstanceRequest) store.BackupPolicy {
	policy := store.BackupPolicy{
		KeepCold:  current.BackupKeepCold,
		KeepHot:   current.BackupKeepHot,
		OnRestart: current.BackupOnRestart,
	}
	if body.BackupKeepCold != nil {
		policy.KeepCold = *body.BackupKeepCold
	}
	if body.BackupKeepHot != nil {
		policy.KeepHot = *body.BackupKeepHot
	}
	if body.BackupOnRestart != nil {
		policy.OnRestart = *body.BackupOnRestart
	}
	return policy
}

// mergePatch validates the body against the row it applies to and produces the update. 03
// §1.3's three rules are checked on the merged result, not the body: a password valid on its
// own can still be a substring of an unmentioned server name. 08 §5.1 checks them again at
// container creation (G2).
func (h *Instances) mergePatch(
	w http.ResponseWriter, r *http.Request, current *store.Instance, body *patchInstanceRequest,
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

	for field, v := range map[string]*int{
		"backup_keep_cold": body.BackupKeepCold, "backup_keep_hot": body.BackupKeepHot,
	} {
		if v != nil && *v < 0 {
			val.Add(field, apierr.FieldOutOfRange, "Keep a whole number of backups, or 0 to keep every one.")
		}
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

// patchPassword resolves the password the merged row should carry. An unchanged one is
// decrypted only to check 03 §1.3 rule 2 against a possibly renamed server, then its stored
// envelope is written back untouched.
func (h *Instances) patchPassword(
	w http.ResponseWriter, r *http.Request, current *store.Instance,
	body *patchInstanceRequest, serverName string, val *apierr.Validation,
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

// patch handles PATCH /instances/{id}, the launch config, gated per field: instance.limits for
// the resource limits, instance.extra_args for the argv tail, instance.settings for the rest. A
// change takes effect on the next start, which rebuilds the container if the row no longer
// describes it (ADR-118).
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

	patch, ok := h.mergePatch(w, r, current, &body)
	if !ok {
		return
	}
	if err := h.DB.UpdateInstanceLaunch(r.Context(), id, &patch); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	// A separate statement, because these take effect immediately and must not set
	// restart_required — an operator told to restart for a change no restart applies is
	// being told something false.
	if body.backupPolicy() {
		if err := h.DB.UpdateInstanceBackupPolicy(r.Context(), id, mergeBackupPolicy(current, &body)); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
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

// password is GET /instances/{id}/password (11 §9): a live game secret readable by any caller
// with instance.view, kept out of every other payload, and audited on every read.
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

// acknowledge is POST /instances/{id}/acknowledge (12 §2.4), the only way out of `error`. It
// re-runs reconciliation for this one instance and lands on the state Docker supports, rather
// than clearing the flag.
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
