package api

import (
	"errors"
	"net/http"
	"strconv"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// uninstallMod is DELETE /instances/{id}/mods/{full_name} (04 §3): remove a package's
// files, driven by the manifest recorded when it was installed and by nothing else (B9).
func (m *Mods) uninstallMod(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := m.mustLoadEditableInstance(w, r, id)
	if !ok {
		return
	}
	removeOrphans, err := parseRemoveOrphans(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	names, err := m.planner().RemovalSet(r.Context(), id, r.PathValue("full_name"), removeOrphans)
	if err != nil {
		writeRemovalError(w, r, err)
		return
	}

	job, err := m.installer().SubmitUninstall(r.Context(), inst, names, u.ID,
		jobAudit(r.Context(), u.ID, id, "instances.mods.uninstall", map[string]any{"full_names": names}))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// parseRemoveOrphans reads the one query parameter. Absent is false: a dependency nothing
// asked for is *offered* for removal, never taken silently, because the panel cannot tell
// a package pulled in as a dependency from one the admin has since come to rely on.
func parseRemoveOrphans(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("remove_orphans")
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apierr.New(errcode.InvalidParameter).With("parameter", "remove_orphans").Wrap(err)
	}
	return v, nil
}

// writeRemovalError maps the two answers an uninstall request can be refused with onto the
// registry: a mod that is not there, and one another mod needs.
func writeRemovalError(w http.ResponseWriter, r *http.Request, err error) {
	var notInstalled *manager.NotInstalledError
	if errors.As(err, &notInstalled) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var required *manager.RequiredByError
	if errors.As(err, &required) {
		apierr.Write(w, r, apierr.New(errcode.ModConflict).
			With("required_by", required.By).
			Wrap(err))
		return
	}
	apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
}

// modPatchRequest is PATCH /instances/{id}/mods/{full_name}'s body. Every field is optional
// and a nil one is left alone, so a client that knows about one field cannot blank another by
// omitting it.
type modPatchRequest struct {
	Side    *string `json:"side"`
	Enabled *bool   `json:"enabled"`
	Locked  *bool   `json:"locked"`
}

// sides is 04 §2's CHECK constraint, restated where the request is validated so a bad value
// is a 422 naming the field rather than a constraint violation surfacing as a 500.
var sides = map[string]bool{
	"server_only": true, "client_required": true, "client_optional": true, store.SideUnknown: true,
}

// decodeModPatch reads and validates the body. A request that sets nothing is refused
// rather than answered with an unchanged row: it is a client that thinks it changed
// something, which is the shape of failure ADR-050's unknown-field rejection exists for.
func decodeModPatch(w http.ResponseWriter, r *http.Request) (modPatchRequest, bool) {
	var body modPatchRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	var val apierr.Validation
	if body.Side == nil && body.Enabled == nil && body.Locked == nil {
		val.Add("side", apierr.FieldRequired, "Give at least one of side, enabled or locked.")
	}
	if body.Side != nil && !sides[*body.Side] {
		val.Add("side", apierr.FieldInvalid,
			"side is one of server_only, client_required, client_optional, unknown.")
	}
	// A label is a row edit answered with the row; enabling moves files and is answered with a
	// job. One request cannot be both.
	if body.Enabled != nil && (body.Side != nil || body.Locked != nil) {
		val.Add("enabled", apierr.FieldInvalid,
			"Change enabled on its own: it moves the mod's files and runs as a job.")
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	return body, true
}

// patchMod is PATCH /instances/{id}/mods/{full_name} (04 §3): the admin's own labels on an
// installed mod.
//
// A `side` carries down the dependency closure, since a client that needs a mod needs what
// that mod needs, and tagging a closure by hand is where an operator ships a client profile
// missing one package (ADR-175). It only ever raises a tag: the strongest claim on a shared
// dependency is the true one.
//
// `side` is set here and nowhere else, since Thunderstore metadata does not reliably encode
// whether a mod is needed on the client (03 §5.6) and a guess would produce a client manifest
// omitting a required one.
//
// `locked` holds the mod at its installed version: Update all skips it, and an install that
// would move it is refused with a conflict instead. It changes no file, so it is a label too,
// and every change is written to the audit log.
//
// `enabled` is not a label: it moves the package's files in or out of server/ (Q37), so it is
// answered with a job, needs a stopped server, and is sent on its own.
func (m *Mods) patchMod(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	if !m.mustLoadTaggableInstance(w, r, id) {
		return
	}

	body, ok := decodeModPatch(w, r)
	if !ok {
		return
	}

	fullName := r.PathValue("full_name")
	if body.Enabled != nil {
		m.submitToggle(w, r, u, id, fullName, *body.Enabled)
		return
	}
	if body.Locked != nil && !m.setLocked(w, r, u, id, fullName, *body.Locked) {
		return
	}
	if body.Side != nil && !m.setSide(w, r, id, fullName, *body.Side) {
		return
	}
	mods, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	for i := range mods {
		if mods[i].FullName == fullName {
			// No load status on a PATCH response: it changes no file, so re-reading
			// BepInEx's log to answer a tag edit would be work for an answer nobody asked
			// this endpoint for. GET /instances/{id}/mods is where that lives.
			pkg, err := m.indexedPackage(r.Context(), fullName, mods[i].Source, nil)
			if err != nil {
				apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
				return
			}
			JSON(w, r, http.StatusOK, toInstalledModView(&mods[i], pkg, nil))
			return
		}
	}
	apierr.Write(w, r, apierr.New(errcode.NotFound))
}

// setLocked writes a version lock and its audit entry. It writes the response and reports false
// when the request cannot go ahead.
func (m *Mods) setLocked(
	w http.ResponseWriter, r *http.Request, u *store.User, id, fullName string, locked bool,
) bool {
	version, _, installed, err := m.DB.InstanceModVersion(r.Context(), id, fullName)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if !installed {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	found, err := m.DB.SetInstanceModLocked(r.Context(), id, fullName, locked)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if !found {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	action := "instances.mods.unlock"
	if locked {
		action = "instances.mods.lock"
	}
	if err := m.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: action, IP: clientIP(r.Context()),
		Detail: detailJSON(map[string]string{"full_name": fullName, "version": version}),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	return true
}

// setSide writes a side tag and raises it down the mod's dependency closure. It writes the
// response and reports false when the request cannot go ahead.
func (m *Mods) setSide(w http.ResponseWriter, r *http.Request, id, fullName, side string) bool {
	found, err := m.DB.SetInstanceModSide(r.Context(), id, fullName, side)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if !found {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	mods, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	raise, err := m.planner().DependenciesToRaise(r.Context(), mods, fullName, side)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if err := m.DB.RaiseInstanceModSides(r.Context(), id, raise, side); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	return true
}

// mustLoadTaggableInstance is patchMod's preamble. It deliberately does not require the
// server to be stopped: `side` is a label nothing on disk or in the container reads, so the
// reason install and uninstall wait for a stopped server — BepInEx reads the plugin directory
// once at startup (B11, C19) — does not apply to it. `enabled` does move files, and its own
// path adds the stopped check. An outstanding definition step still blocks, since the chain
// reinstalls the mods whose tags these are (ADR-164).
//
// Writes the response and reports false when the request cannot go ahead.
func (m *Mods) mustLoadTaggableInstance(w http.ResponseWriter, r *http.Request, id string) bool {
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return false
	}
	return operationSettled(w, r, m.DB, id)
}

// mustLoadEditableInstance is the preamble the two mod-writing endpoints share: the instance,
// stopped, and owing no outstanding definition step. BepInEx reads the plugin directory once at
// startup, so changing it under a running server does nothing until a restart and risks pulling
// a file out from under a process holding it open (B11, C19); an open operation would overwrite
// the change with the definition the chain still owes (ADR-164). patchMod writes no file and
// has a lighter preamble of its own.
//
// Writes the response and reports false when the request cannot go ahead.
func (m *Mods) mustLoadEditableInstance(
	w http.ResponseWriter, r *http.Request, id string,
) (*store.Instance, bool) {
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, false
	}
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return nil, false
	}
	if !operationSettled(w, r, m.DB, id) {
		return nil, false
	}
	return inst, true
}
