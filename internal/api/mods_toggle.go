package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

// Disabling a mod without uninstalling it (Q37) is what an operator does while hunting the mod
// that breaks their server: switch one off, start, see, switch it back. It has to be real, so
// it moves the package's loadable files out of server/, where BepInEx reads them, into the
// instance's parking tree, and records each move on the file manifest. Uninstall, update-all,
// the game update's replay and the crash sweep all read those flags, so B9's exact uninstall
// holds for a disabled package too.

// modTogglePayload is the job's persisted arguments. The sweep needs no staging directory: the
// row, unchanged until the job's Finish transaction, already records where every file belongs.
type modTogglePayload struct {
	FullName string `json:"full_name"`
	Enable   bool   `json:"enable"`
}

// parkedPackageDir is one package's directory in the instance's parking tree. The full name
// passed installer.CheckFullName when the package was installed.
func parkedPackageDir(inst *store.Instance, fullName string) string {
	return filepath.Join(instance.ParkedModsDir(inst.DataDir), fullName)
}

// toggleRefusal is a disable or enable the dependency graph does not allow. It is a 409 naming
// the packages in the way, like an uninstall another mod still needs.
type toggleRefusal struct {
	detail string
	names  []string
	reason string
}

func (e *toggleRefusal) Error() string { return e.reason }

// checkToggle decides whether fullName may move to enable. Disabling is refused for the mod
// loader itself, and while an enabled mod depends on the package; enabling is refused while the
// package depends on a disabled one. Either would leave an enabled mod BepInEx cannot load, and
// the operator would be hunting a failure the panel made.
func (m *Mods) checkToggle(
	ctx context.Context, rows []store.InstanceMod, fullName string, enable bool,
) error {
	if !enable && fullName == BepInExPack {
		return &toggleRefusal{
			detail: "reason", reason: "BepInEx is the mod loader; disabling it disables every mod. " +
				"Disable the mods themselves instead.",
		}
	}
	needs, err := m.dependencyEdges(ctx, rows)
	if err != nil {
		return err
	}
	enabled := make(map[string]bool, len(rows))
	installed := make(map[string]bool, len(rows))
	for i := range rows {
		installed[rows[i].FullName] = true
		enabled[rows[i].FullName] = rows[i].Enabled
	}

	if !enable {
		remaining := map[string]bool{}
		for name, on := range enabled {
			if on && name != fullName {
				remaining[name] = true
			}
		}
		if by := requiredBy(fullName, remaining, needs); len(by) > 0 {
			return &toggleRefusal{
				detail: "required_by", names: by,
				reason: fmt.Sprintf("%s is needed by %v, which are enabled", fullName, by),
			}
		}
		return nil
	}

	var off []string
	for _, dep := range needs[fullName] {
		if installed[dep] && !enabled[dep] && !slices.Contains(off, dep) {
			off = append(off, dep)
		}
	}
	sort.Strings(off)
	if len(off) > 0 {
		return &toggleRefusal{
			detail: "disabled", names: off,
			reason: fmt.Sprintf("%s needs %v, which are disabled", fullName, off),
		}
	}
	return nil
}

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
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	var row *store.InstanceMod
	for i := range rows {
		if rows[i].FullName == fullName {
			row = &rows[i]
		}
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if row.Enabled == enable {
		m.writeModRow(w, r, row)
		return
	}
	if err := m.checkToggle(r.Context(), rows, fullName, enable); err != nil {
		var refusal *toggleRefusal
		if errors.As(err, &refusal) {
			e := apierr.New(apierr.ModConflict)
			if refusal.names != nil {
				e = e.With(refusal.detail, refusal.names)
			} else {
				e = e.With(refusal.detail, refusal.reason)
			}
			apierr.Write(w, r, e.Wrap(err))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	payload := modTogglePayload{FullName: fullName, Enable: enable}
	job, err := m.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindModToggle, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID, Payload: payload,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := holdStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim mod_toggle for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, m.runModToggle(inst, payload))
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
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toInstalledModView(row, pkg, nil))
}

// modToggleCancelPolicy: never. The job is a handful of file moves, and stopping between two of
// them would leave a package half-loadable, which is worse than either state being chosen.
func modToggleCancelPolicy(string) (cancellable bool, phase string) {
	return false, "moving the mod's files"
}

// runModToggle is the mod_toggle Runner: move the files, then flip the row in the Finish
// transaction. A failure part-way settles every file back to where the unchanged row says it is.
func (m *Mods) runModToggle(inst *store.Instance, payload modTogglePayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		row, manifest, err := toggleRow(ctx, m.DB, inst.ID, payload.FullName)
		if err != nil {
			return modJobFailed(apierr.Internal, err)
		}
		if row.Enabled == payload.Enable {
			h.Progress(ctx, 100, "already in that state; nothing to do")
			return jobs.Outcome{Status: jobs.StatusSucceeded}
		}
		next, err := moveToggledFiles(ctx, h, inst, payload, manifest)
		if err != nil {
			return m.settleToggle(ctx, inst, payload.FullName, manifest, err)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return m.settleToggle(ctx, inst, payload.FullName, manifest, err)
		}
		return finishToggle(ctx, h, inst, payload, string(raw))
	}
}

// moveToggledFiles does the one move a toggle is, and returns the manifest that records it.
func moveToggledFiles(
	ctx context.Context, h *jobs.Handle, inst *store.Instance,
	payload modTogglePayload, manifest []installer.ManifestEntry,
) ([]installer.ManifestEntry, error) {
	serverRoot, parkDir := serverDir(inst), parkedPackageDir(inst, payload.FullName)
	if payload.Enable {
		h.Progress(ctx, 30, "putting the mod's files back")
		if _, err := installer.Unpark(installer.ParkedPaths(manifest), parkDir, serverRoot); err != nil {
			return nil, fmt.Errorf("enable %s: %w", payload.FullName, err)
		}
		return installer.MarkParked(manifest, nil), nil
	}
	h.Progress(ctx, 30, "moving the mod's files out of the server")
	moved, err := installer.Park(installer.Movable(manifest), serverRoot, parkDir)
	if err != nil {
		return nil, fmt.Errorf("disable %s: %w", payload.FullName, err)
	}
	h.Log(fmt.Sprintf("%s: %d files moved out of the server", payload.FullName, len(moved)))
	return installer.MarkParked(manifest, moved), nil
}

// finishToggle is the successful outcome: the row and restart_required in the Finish
// transaction, and for an enable, the emptied parking directory once they have committed.
func finishToggle(
	ctx context.Context, h *jobs.Handle, inst *store.Instance, payload modTogglePayload, manifest string,
) jobs.Outcome {
	verb := "disabled"
	if payload.Enable {
		verb = "enabled"
	}
	h.Progress(ctx, 100, fmt.Sprintf("%s %s", verb, payload.FullName))
	out := jobs.Outcome{
		Status: jobs.StatusSucceeded,
		OnFinish: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxSetInstanceModEnabled(
				ctx, tx, inst.ID, payload.FullName, payload.Enable, manifest); err != nil {
				return fmt.Errorf("record the toggle: %w", err)
			}
			if err := store.TxSetRestartRequired(ctx, tx, inst.ID); err != nil {
				return fmt.Errorf("record the toggle: %w", err)
			}
			return nil
		},
	}
	if payload.Enable {
		// Only empty directories remain once every parked file is back; removed after the row
		// commits, so a failed Finish still finds the tree the sweep settles against.
		parkDir := parkedPackageDir(inst, payload.FullName)
		out.AfterFinish = func(context.Context) { _ = os.RemoveAll(parkDir) }
	}
	return out
}

// toggleRow reads the package's row and decodes its manifest.
func toggleRow(
	ctx context.Context, db *store.DB, instanceID, fullName string,
) (*store.InstanceMod, []installer.ManifestEntry, error) {
	rows, err := db.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read installed mods: %w", err)
	}
	for i := range rows {
		if rows[i].FullName != fullName {
			continue
		}
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(rows[i].FileManifest), &manifest); err != nil {
			return nil, nil, fmt.Errorf("read the manifest of %s: %w", fullName, err)
		}
		return &rows[i], manifest, nil
	}
	return nil, nil, fmt.Errorf("%s is no longer installed", fullName)
}

// settleToggle is a failed toggle's undo: every file goes back to where the manifest, which the
// job never changed, records it.
func (m *Mods) settleToggle(
	ctx context.Context, inst *store.Instance, fullName string,
	manifest []installer.ManifestEntry, cause error,
) jobs.Outcome {
	if err := installer.Settle(manifest, serverDir(inst), parkedPackageDir(inst, fullName)); err != nil {
		slog.ErrorContext(ctx, "mod toggle undo incomplete",
			slog.String("instance_id", inst.ID), slog.String("full_name", fullName),
			slog.Any("error", err))
		return modJobFailed(apierr.Internal,
			fmt.Errorf("%w; and these files could not be put back: %w", cause, err))
	}
	return modJobFailed(apierr.Internal, cause)
}

// disabledInClosure names the disabled packages a resolved closure touches. An install or update
// that reaches one is refused: updating a parked package would place files next to the ones it
// parked, and a new mod depending on a disabled one would not load.
func disabledInClosure(names []string, installed []store.InstanceMod) []string {
	off := map[string]bool{}
	for i := range installed {
		if !installed[i].Enabled {
			off[installed[i].FullName] = true
		}
	}
	var out []string
	for _, name := range names {
		if off[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// closureNames is every package a resolved closure names, no-ops included: a new mod whose
// dependency is installed but disabled is as unloadable as one whose dependency is missing.
func closureNames(closure modresolver.Closure) []string {
	out := make([]string, 0, len(closure.Nodes))
	for _, n := range closure.Nodes {
		out = append(out, n.FullName)
	}
	return out
}

// refuseDisabled is the dry runs' check: the disabled packages a closure touches, read against
// what is installed now.
func (m *Mods) refuseDisabled(
	ctx context.Context, instanceID string, closure modresolver.Closure,
) ([]string, error) {
	rows, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	return disabledInClosure(closureNames(closure), rows), nil
}

// writeDisabledConflict is the 409 an install or update touching a disabled package answers.
func writeDisabledConflict(w http.ResponseWriter, r *http.Request, off []string) {
	apierr.Write(w, r, apierr.New(apierr.ModConflict).With("disabled", off))
}
