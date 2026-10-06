package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"unicode/utf8"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// optionalFloat64 distinguishes an absent request field from an explicit null.
type optionalFloat64 struct {
	set   bool
	value *float64
}

func (o *optionalFloat64) UnmarshalJSON(data []byte) error {
	o.set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.value = nil
		return nil
	}
	var value float64
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode optional float: %w", err)
	}
	o.value = &value
	return nil
}

// Instances serves the instance surface: creation, the read-side CRUD, the launch-config
// PATCH, the audited password endpoint, acknowledge, and the lifecycle jobs in lifecycle.go.
type Instances struct {
	Snapshotter *control.Snapshotter
	Operations  *control.Operations
	DB          *store.DB
	Authz       *authz.Authz
	Runtime     runtime.Runtime
	Keeper      *crypto.Keeper
	Engine      *jobs.Engine
	Cfg         *config.Config
	// Streams holds one log reader and one stats sampler per running instance, plus the ring
	// buffer each reader fills (14 §1). It is the source for the console and stats topics and
	// for jobs waiting on a matched line.
	Streams  *instance.Streams
	Commands *command.Manager

	// Mods is the create wizard's hook into the mod engine (Q42). Nil in a panel with no mod
	// engine, where create refuses a request that names mods rather than provisioning a
	// vanilla server.
	Mods ModEngine

	// removeAll is replaced only by deletion failure tests.
	removeAll func(string) error
	// setupApply is replaced only by restore failure tests.
	setupApply func(*store.Instance, string, map[string]map[string]bool, map[string]map[string]bool) error
	// Notify is the notification fan-out. It is wired after both are built, the way Mods is,
	// and is nil in a test that does not exercise notifications.
	Notify NotificationSink
}

// NotificationSink records the domain events emitted by instance work.
type NotificationSink interface {
	NotifyUnexpectedStop(context.Context, *store.Instance, string, string)
	NotifyPublicBuild(context.Context, string, string) func(context.Context, *sql.Tx) error
	DispatchAlerts(context.Context)
}

// ModEngine is the slice of the mod engine the create path needs, declared by the consumer
// (06 §4). Mods satisfies it and is wired in router.go.
type ModEngine interface {
	// CheckResolvable reports whether one requested package's whole closure can be computed
	// from the cached index, writing and downloading nothing. inst may describe an instance
	// that does not exist yet, in which case the answer is a fresh server's closure.
	CheckResolvable(ctx context.Context, inst *store.Instance, req manager.PackageRequest) error

	// StageReplay materialises every installed package's manifested files into dest, taken
	// from the cached package archives and placed by recorded hash. A game update calls it to
	// rebuild the instance's mod layer on a fresh clone (ADR-138).
	StageReplay(ctx context.Context, inst *store.Instance, dest string) error

	// SubmitInstall queues one mod_install job, running afterFinish only if it succeeds.
	SubmitInstall(
		ctx context.Context,
		inst *store.Instance,
		req manager.PackageRequest,
		requestedBy string,
		afterFinish func(context.Context),
	) (*store.Job, error)
}

func instanceRoutes(rt *routeTable, h *Instances) {
	rt.Handle("GET /api/v1/instances", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/instances", http.HandlerFunc(h.create))
	rt.Large("POST /api/v1/instances/import", http.HandlerFunc(h.importManifest))
	rt.Large("POST /api/v1/instances/manifest/preview", http.HandlerFunc(h.previewManifest))
	rt.Handle("GET /api/v1/instances/{id}/manifest", http.HandlerFunc(h.exportManifest))
	// Registered ahead of /instances/{id}, which ServeMux would resolve the same way.
	rt.Handle("GET /api/v1/instances/orphans", http.HandlerFunc(h.orphans))
	rt.Handle("GET /api/v1/instances/inbox", http.HandlerFunc(h.inbox))
	rt.Handle("GET /api/v1/orphans/{container_id}", http.HandlerFunc(h.previewAdoption))
	rt.Handle("POST /api/v1/orphans/{container_id}", http.HandlerFunc(h.adopt))
	rt.Handle("GET /api/v1/game/options", http.HandlerFunc(h.options))
	rt.Handle("GET /api/v1/instances/{id}", http.HandlerFunc(h.get))
	rt.Handle("GET /api/v1/instances/{id}/update-status", http.HandlerFunc(h.updateStatus))
	rt.Handle("PATCH /api/v1/instances/{id}", http.HandlerFunc(h.patch))
	rt.Handle("GET /api/v1/instances/{id}/password", http.HandlerFunc(h.password))
	rt.Handle("GET /api/v1/instances/{id}/logs", http.HandlerFunc(h.logs))
	rt.Handle("GET /api/v1/instances/{id}/stats", http.HandlerFunc(h.stats))
	rt.Handle("POST /api/v1/instances/{id}/commands", http.HandlerFunc(h.command))
	rt.Handle("GET /api/v1/instances/{id}/jobs", http.HandlerFunc(h.jobHistory))
	rt.Handle("GET /api/v1/instances/{id}/disk", http.HandlerFunc(h.disk))
	rt.Handle("GET /api/v1/instances/{id}/backups", http.HandlerFunc(h.listBackups))
	rt.Handle("POST /api/v1/instances/{id}/backups", http.HandlerFunc(h.createBackup))
	rt.Handle("DELETE /api/v1/instances/{id}/backups/{bid}", http.HandlerFunc(h.deleteBackup))
	rt.Handle("POST /api/v1/instances/{id}/backups/{bid}/restore", http.HandlerFunc(h.restoreBackup))
	rt.Handle("POST /api/v1/instances/{id}/acknowledge", http.HandlerFunc(h.acknowledge))
	rt.Handle("GET /api/v1/instances/{id}/operation", http.HandlerFunc(h.operation))
	rt.Handle("POST /api/v1/instances/{id}/operation/resume", http.HandlerFunc(h.resumeOperation))
	rt.Handle("POST /api/v1/instances/{id}/operation/abandon", http.HandlerFunc(h.abandonOperation))
	rt.Handle("POST /api/v1/instances/{id}/start", http.HandlerFunc(h.start))
	rt.Handle("POST /api/v1/instances/{id}/stop", http.HandlerFunc(h.stop))
	rt.Handle("POST /api/v1/instances/{id}/restart", http.HandlerFunc(h.restart))
	rt.Handle("POST /api/v1/instances/{id}/clone", http.HandlerFunc(h.clone))
	rt.Handle("POST /api/v1/instances/{id}/update", http.HandlerFunc(h.updateGame))
	rt.Handle("DELETE /api/v1/instances/{id}", http.HandlerFunc(h.delete))
	h.listRoutes(rt)
	h.configRoutes(rt)
	h.setupRoutes(rt)
	// Stream, not Handle: 11 §8.1's 30 s TimeoutHandler would sever a large upload
	// mid-transfer.
	rt.Handle("GET /api/v1/instances/{id}/worlds", http.HandlerFunc(h.listWorlds))
	rt.LargeStream("POST /api/v1/instances/{id}/worlds/import", http.HandlerFunc(h.importWorld))
	rt.Handle("POST /api/v1/instances/{id}/worlds/{name}/restore", http.HandlerFunc(h.restoreWorldFromDisk))
	rt.Handle("DELETE /api/v1/instances/{id}/worlds/{name}", http.HandlerFunc(h.deleteWorld))
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if all {
		ids = nil // ListInstances(nil) is "every row"; VisibleInstances(all=true) carries none
	}
	instances, err := h.DB.ListInstances(r.Context(), ids)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
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
	CPULimit   optionalFloat64    `json:"cpu_limit"`
	ExtraArgs  *string            `json:"extra_args"`
	// Backup retention and the restart archive. Not launch fields: they shape no container,
	// so changing one sets no restart_required (ADR-118's drift check would not see it
	// either).
	BackupKeepCold      *int  `json:"backup_keep_cold"`
	BackupKeepHot       *int  `json:"backup_keep_hot"`
	BackupOnRestart     *bool `json:"backup_on_restart"`
	RemoteBackupEnabled *bool `json:"remote_backup_enabled"`
	RemoteKeepCold      *int  `json:"remote_keep_cold"`
	RemoteKeepHot       *int  `json:"remote_keep_hot"`
	RemoteKeepSnapshots *int  `json:"remote_keep_snapshots"`
	// StatusPublished opts this instance into the unauthenticated status route. Not a launch
	// field either: it shapes no container.
	StatusPublished *bool `json:"status_published"`
	// StatusNotice and StatusConnectInfo are the status page's plain text, trimmed and at most
	// maxStatusText characters each.
	StatusNotice      *string `json:"status_notice"`
	StatusConnectInfo *string `json:"status_connect_info"`
}

// maxStatusText bounds each of the status page's text fields, in characters.
const maxStatusText = 500

// launch reports whether the body touches anything that shapes a container. Nothing else may
// set restart_required: telling an operator to restart for a change no restart applies is
// telling them something false.
func (b *patchInstanceRequest) launch() bool {
	return b.ServerName != nil || b.Password != nil || b.Public != nil || b.Crossplay != nil ||
		b.Preset != nil || b.Modifiers != nil || b.MemLimitMB != nil || b.CPULimit.set ||
		b.ExtraArgs != nil
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
	if b.MemLimitMB != nil || b.CPULimit.set {
		need = append(need, authz.InstanceLimits)
	}
	if b.ExtraArgs != nil {
		need = append(need, authz.InstanceExtraArgs)
	}
	if b.ServerName != nil || b.Password != nil || b.Public != nil ||
		b.Crossplay != nil || b.Preset != nil || b.Modifiers != nil || b.backupPolicy() ||
		b.StatusPublished != nil || b.StatusNotice != nil || b.StatusConnectInfo != nil || b.remotePolicy() {
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
	if body.CPULimit.set {
		patch.CPULimit = body.CPULimit.value
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

// addBackupPolicyViolations rejects a negative retention count.
func addBackupPolicyViolations(val *apierr.Validation, body *patchInstanceRequest) {
	for _, f := range []struct {
		field string
		value *int
	}{
		{"backup_keep_cold", body.BackupKeepCold},
		{"backup_keep_hot", body.BackupKeepHot},
		{"remote_keep_cold", body.RemoteKeepCold},
		{"remote_keep_hot", body.RemoteKeepHot},
		{"remote_keep_snapshots", body.RemoteKeepSnapshots},
	} {
		if f.value != nil && *f.value < 0 {
			val.Add(f.field, apierr.FieldOutOfRange, "Keep a whole number of backups, or 0 to keep every one.")
		}
	}
}

// addStatusTextViolations rejects status page text longer than maxStatusText characters.
func addStatusTextViolations(val *apierr.Validation, body *patchInstanceRequest) {
	for _, f := range []struct {
		field string
		value *string
	}{{"status_notice", body.StatusNotice}, {"status_connect_info", body.StatusConnectInfo}} {
		if f.value != nil && utf8.RuneCountInString(strings.TrimSpace(*f.value)) > maxStatusText {
			val.Add(f.field, apierr.FieldInvalid,
				fmt.Sprintf("Use at most %d characters.", maxStatusText))
		}
	}
}

// mergeStatusText is PATCH semantics for the status page text: each starts from the row, and
// what body set replaces it, trimmed.
func mergeStatusText(current *store.Instance, body *patchInstanceRequest) (notice, connectInfo string) {
	notice, connectInfo = current.StatusNotice, current.StatusConnectInfo
	if body.StatusNotice != nil {
		notice = strings.TrimSpace(*body.StatusNotice)
	}
	if body.StatusConnectInfo != nil {
		connectInfo = strings.TrimSpace(*body.StatusConnectInfo)
	}
	return notice, connectInfo
}

// mergePatch validates the body against the row it applies to and produces the update and the
// launch fields it changes. The password rules are checked on the merged result, not the body:
// a password valid on its own can still be a substring of an unmentioned server name.
func (h *Instances) mergePatch(
	w http.ResponseWriter, r *http.Request, current *store.Instance, body *patchInstanceRequest,
) (store.InstanceLaunch, []change, bool) {
	var val apierr.Validation

	serverName := current.ServerName
	if body.ServerName != nil {
		serverName = *body.ServerName
	}
	password, passwordChanged, ok := h.patchPassword(w, r, current, body, serverName, &val)
	if !ok {
		return store.InstanceLaunch{}, nil, false
	}

	addBackupPolicyViolations(&val, body)
	addStatusTextViolations(&val, body)

	patch := mergeInstanceLaunch(current, body, password)
	for _, v := range instance.ValidateResources(patch.MemLimitMB, patch.CPULimit) {
		addResourceViolation(&val, v)
	}
	if body.Modifiers != nil {
		encoded, err := encodeModifiers(*body.Modifiers)
		if err != nil {
			val.Add("modifiers", apierr.FieldInvalid, "Modifiers must be a flat object of strings.")
		}
		patch.Modifiers = &encoded
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return store.InstanceLaunch{}, nil, false
	}
	changes := launchChanges(current, body, &patch, passwordChanged)
	return patch, changes, true
}

// fieldChange appends a change for field when the value differs.
func fieldChange[T comparable](out []change, field string, from, to T) []change {
	if from == to {
		return out
	}
	return append(out, change{Field: field, From: from, To: to})
}

// pointerChange is fieldChange for a nullable field. A nil side is left out of the change.
func pointerChange[T comparable](out []change, field string, from, to *T) []change {
	if (from == nil && to == nil) || (from != nil && to != nil && *from == *to) {
		return out
	}
	c := change{Field: field}
	if from != nil {
		c.From = *from
	}
	if to != nil {
		c.To = *to
	}
	return append(out, c)
}

// launchChanges lists the launch fields patch changes, named as the PATCH body names them. The
// password is recorded as changed and never by value.
func launchChanges(
	current *store.Instance, body *patchInstanceRequest, patch *store.InstanceLaunch, passwordChanged bool,
) []change {
	var out []change
	out = fieldChange(out, "server_name", current.ServerName, patch.ServerName)
	if passwordChanged {
		out = append(out, change{Field: "password", Secret: true})
	}
	out = fieldChange(out, "public", current.Public, patch.Public)
	out = fieldChange(out, "crossplay", current.Crossplay, patch.Crossplay)
	out = pointerChange(out, "preset", current.Preset, patch.Preset)
	if body.Modifiers != nil {
		was, is := decodeModifiers(current.Modifiers), *body.Modifiers
		if !maps.Equal(was, is) {
			out = append(out, change{Field: "modifiers", From: was, To: is})
		}
	}
	out = fieldChange(out, "mem_limit_mb", current.MemLimitMB, patch.MemLimitMB)
	out = pointerChange(out, "cpu_limit", current.CPULimit, patch.CPULimit)
	return pointerChange(out, "extra_args", current.ExtraArgs, patch.ExtraArgs)
}

// decodeModifiers reads the stored modifiers, which are empty for an unset or unreadable value.
func decodeModifiers(raw *string) map[string]string {
	m := map[string]string{}
	if raw != nil && *raw != "" {
		_ = json.Unmarshal([]byte(*raw), &m)
	}
	return m
}

// backupPolicyChanges lists the retention fields policy changes, named as the PATCH body names
// them.
func backupPolicyChanges(current *store.Instance, policy store.BackupPolicy) []change {
	var out []change
	out = fieldChange(out, "backup_keep_cold", current.BackupKeepCold, policy.KeepCold)
	out = fieldChange(out, "backup_keep_hot", current.BackupKeepHot, policy.KeepHot)
	return fieldChange(out, "backup_on_restart", current.BackupOnRestart, policy.OnRestart)
}

// patchPassword resolves the password the merged row should carry. An unchanged one is
// decrypted only to check 03 §1.3 rule 2 against a possibly renamed server, then its stored
// envelope is written back untouched. changed reports whether the body set a different one.
func (h *Instances) patchPassword(
	w http.ResponseWriter, r *http.Request, current *store.Instance,
	body *patchInstanceRequest, serverName string, val *apierr.Validation,
) (envelope string, changed, ok bool) {
	stored, err := h.DB.InstancePassword(r.Context(), current.ID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return "", false, false
	}
	location := crypto.InstancePasswordLocation(current.ID)
	plaintext, err := h.Keeper.Decrypt(crypto.PurposeInstancePassword, location, stored)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return "", false, false
	}
	envelope = stored
	unchanged := body.Password == nil || *body.Password == string(plaintext)
	if body.Password != nil {
		plaintext = []byte(*body.Password)
	}

	for _, v := range instance.ValidateLaunch(serverName, current.WorldName, string(plaintext)) {
		addLaunchViolation(val, v)
	}
	if val.Err() != nil || unchanged {
		return envelope, false, true
	}
	if envelope, err = h.Keeper.Encrypt(crypto.PurposeInstancePassword, location, plaintext); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return "", false, false
	}
	return envelope, true, true
}

// applySettings writes the launch and retention fields the body sets and records what changed
// as one audit entry, none when nothing did. It reports false after writing the error.
func (h *Instances) applySettings(
	w http.ResponseWriter, r *http.Request, u *store.User, current *store.Instance, body *patchInstanceRequest,
) bool {
	var changes []change
	if body.launch() {
		patch, launch, ok := h.mergePatch(w, r, current, body)
		if !ok {
			return false
		}
		if err := h.DB.UpdateInstanceLaunch(r.Context(), current.ID, &patch); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
		changes = launch
	} else {
		// mergePatch checks retention and status text along with the launch fields; without
		// launch fields they are checked here.
		var val apierr.Validation
		addBackupPolicyViolations(&val, body)
		addStatusTextViolations(&val, body)
		if err := val.Err(); err != nil {
			apierr.Write(w, r, err)
			return false
		}
	}
	// A separate statement, because these take effect immediately and must not set
	// restart_required — an operator told to restart for a change no restart applies is
	// being told something false.
	if body.backupPolicy() {
		policy := mergeBackupPolicy(current, body)
		if err := h.DB.UpdateInstanceBackupPolicy(r.Context(), current.ID, policy); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
		changes = append(changes, backupPolicyChanges(current, policy)...)
	}
	remoteChanges, ok := h.applyRemotePolicy(w, r, current, body)
	if !ok {
		return false
	}
	changes = append(changes, remoteChanges...)
	if len(changes) > 0 {
		if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
			UserID: u.ID, InstanceID: current.ID, Action: "instances.settings.update",
			Detail: detailJSON(map[string]any{"changes": changes}), IP: clientIP(r.Context()),
		}); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
	}
	return true
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}

	var body patchInstanceRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	for _, action := range body.actions() {
		if !h.Authz.Can(r.Context(), u, action, id) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}

	current, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}

	if !h.applySettings(w, r, u, current, &body) {
		return
	}
	if !h.publishStatus(w, r, u, current, &body) {
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}

// publishStatus stores public status fields. Unpublishing runs first, so a later failure
// cannot leave newly submitted text exposed. Publishing runs after the text is stored.
func (h *Instances) publishStatus(
	w http.ResponseWriter, r *http.Request, u *store.User, current *store.Instance, body *patchInstanceRequest,
) bool {
	want := body.StatusPublished
	changed := want != nil && *want != current.StatusPublished
	if changed && !*want && !h.setStatusPublished(w, r, u, current.ID, false) {
		return false
	}

	notice, connectInfo := mergeStatusText(current, body)
	changes := fieldChange(nil, "status_notice", current.StatusNotice, notice)
	changes = fieldChange(changes, "status_connect_info", current.StatusConnectInfo, connectInfo)
	if len(changes) > 0 {
		if err := h.DB.SetInstanceStatusText(r.Context(), current.ID, notice, connectInfo); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
		if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
			UserID: u.ID, InstanceID: current.ID, Action: "instances.status.update",
			Detail: detailJSON(map[string]any{"changes": changes}), IP: clientIP(r.Context()),
		}); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
	}

	return !changed || !*want || h.setStatusPublished(w, r, u, current.ID, true)
}

// setStatusPublished changes the public route's opt-in and records the decision.
func (h *Instances) setStatusPublished(
	w http.ResponseWriter, r *http.Request, u *store.User, id string, publish bool,
) bool {
	if err := h.DB.SetInstanceStatusPublished(r.Context(), id, publish); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	action := "instances.status.unpublished"
	if publish {
		action = "instances.status.published"
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: action, IP: clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	return true
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
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	exists, err := h.DB.InstanceExists(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !exists {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	plaintext, err := control.DecryptPassword(r.Context(), h.DB, h.Keeper, id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.password.read",
		IP: middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, instancePassword{Password: plaintext})
}

// acknowledge is POST /instances/{id}/acknowledge (12 §2.4), the only way out of `error`. It
// re-runs reconciliation for this one instance and lands on the state Docker supports, rather
// than clearing the flag. Releasing a parked instance makes it startable again, so it needs
// instance.start, and the move is audited with its from and to states.
func (h *Instances) acknowledge(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceStart, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if instance.State(inst.State) != instance.StateError {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	if err := instance.ValidateTransition(instance.StateError, next); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	detail, err := json.Marshal(map[string]instance.State{"from": instance.StateError, "to": next})
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if _, err := instance.SetStateAudited(r.Context(), h.DB, id,
		instance.StateError, next, &store.AuditEntry{
			UserID: u.ID, InstanceID: id, Action: "instances.acknowledge", Detail: string(detail),
			IP: middleware.ClientIPFrom(r.Context()).String(),
		}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	updated, err := h.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, updated)
}

func (h *Instances) snapshotter() *control.Snapshotter {
	if h.Snapshotter != nil {
		return h.Snapshotter
	}
	return &control.Snapshotter{DataRoot: h.Cfg.Data.Root, Runtime: h.Runtime}
}
