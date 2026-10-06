package api

import (
	"errors"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// Disabling a mod without uninstalling it (Q37) is what an operator does while hunting the mod
// that breaks their server: switch one off, start, see, switch it back. It has to be real, so
// it moves the package's loadable files out of server/, where BepInEx reads them, into the
// instance's parking tree, and records each move on the file manifest. Uninstall, update-all,
// the game update's replay and the crash sweep all read those flags, so B9's exact uninstall
// holds for a disabled package too.

// submitToggle is the enabled half of PATCH /instances/{id}/mods/{full_name}: a job, because it
// moves files, on a stopped server, because BepInEx reads its plugin directory once at start
// (B11). A request for the state the mod is already in answers the row, not a job.
func (m *Mods) submitToggle(
	w http.ResponseWriter, r *http.Request, u *store.User, id, fullName string, enable bool,
) {
	inst, ok := m.mustLoadEditableInstance(w, r, id)
	if !ok {
		return
	}
	rows, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	var row *store.InstanceMod
	for i := range rows {
		if rows[i].FullName == fullName {
			row = &rows[i]
		}
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if row.Enabled == enable {
		m.writeModRow(w, r, row)
		return
	}
	if err := m.plan.CheckToggle(r.Context(), rows, fullName, enable); err != nil {
		var refusal *manager.ToggleRefusal
		if errors.As(err, &refusal) {
			e := apierr.New(errcode.ModConflict)
			if refusal.Names != nil {
				e = e.With(refusal.Detail, refusal.Names)
			} else {
				e = e.With(refusal.Detail, refusal.Reason)
			}
			apierr.Write(w, r, e.Wrap(err))
			return
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	action := "instances.mods.disable"
	if enable {
		action = "instances.mods.enable"
	}
	job, err := m.install.SubmitToggle(r.Context(), inst, fullName, enable, u.ID,
		jobAudit(r.Context(), u.ID, id, action, map[string]string{"full_name": fullName}))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// writeModRow answers with one installed row, as PATCH does for a label.
func (m *Mods) writeModRow(w http.ResponseWriter, r *http.Request, row *store.InstanceMod) {
	pkg, err := m.indexedPackage(r.Context(), row.FullName, row.Source, nil)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toInstalledModView(row, pkg, nil))
}

// writeDisabledConflict is the 409 an install or update touching a disabled package answers.
func writeDisabledConflict(w http.ResponseWriter, r *http.Request, off []string) {
	apierr.Write(w, r, apierr.New(errcode.ModConflict).With("disabled", off))
}
