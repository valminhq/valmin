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
	"sort"
	"strconv"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/store"
)

// mod_uninstall's checkpoints. `↯` Nothing is removed before `saved`: the job copies every
// file it is going to delete into its staging area first, which is what lets a crash
// half-way through be a restore rather than a partially-removed package (12 §9.4).
const (
	checkpointSaved   = "saved"
	checkpointRemoved = "removed"
)

// modUninstallPayload is the job's persisted arguments. FullNames is the complete removal
// set — the package the user named plus any orphaned dependencies they asked for — resolved
// in the request rather than in the job, so the row records exactly what was authorised.
type modUninstallPayload struct {
	StagingDir string   `json:"staging_dir"`
	FullNames  []string `json:"full_names"`
}

// uninstallMod is DELETE /instances/{id}/mods/{full_name} (04 §3): remove a package's
// files, driven by the manifest recorded when it was installed and by nothing else (B9).
func (m *Mods) uninstallMod(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	// B11 / C19, as for install: BepInEx reads the plugin directory once at startup, so
	// removing a `.dll` under a running server changes nothing until a restart and risks
	// pulling a file out from under a process that has it open.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}
	removeOrphans, err := parseRemoveOrphans(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	names, err := m.removalSet(r.Context(), id, r.PathValue("full_name"), removeOrphans)
	if err != nil {
		writeRemovalError(w, r, err)
		return
	}

	root := modStagingRoot(m.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	staging, err := os.MkdirTemp(root, "uninstall-*")
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	payload := modUninstallPayload{StagingDir: staging, FullNames: names}
	job, err := m.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindModUninstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID, Payload: payload,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateStopped))
			if err != nil {
				return fmt.Errorf("claim mod_uninstall for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, m.runModUninstall(inst, payload))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
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
		return false, apierr.New(apierr.InvalidParameter).With("parameter", "remove_orphans").Wrap(err)
	}
	return v, nil
}

// requiredByError is an uninstall that another installed package depends on.
type requiredByError struct {
	FullName string
	By       []string
}

func (e *requiredByError) Error() string {
	return fmt.Sprintf("%s is required by %v", e.FullName, e.By)
}

// notInstalledError is a full name that is not installed on this instance. It is a 404 and
// not a 422: from the caller's side the resource simply is not there (D2, ADR-038).
type notInstalledError struct{ FullName string }

func (e *notInstalledError) Error() string { return e.FullName + " is not installed" }

// removalSet is what an uninstall will actually remove: the named package, plus — only if
// the request asked — the dependencies it leaves behind that nothing else needs.
//
// `↯` The dependent check is a refusal, not a cascade. Removing `Therzie-Warfare` while
// `Therzie-Armory` needs it would leave Armory installed and unloadable, which looks to the
// admin like the mod broke rather than like the panel broke it.
func (m *Mods) removalSet(
	ctx context.Context, instanceID, fullName string, removeOrphans bool,
) ([]string, error) {
	rows, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	installed := make(map[string]*store.InstanceMod, len(rows))
	for i := range rows {
		installed[rows[i].FullName] = &rows[i]
	}
	if installed[fullName] == nil {
		return nil, &notInstalledError{FullName: fullName}
	}

	needs, err := m.dependencyEdges(ctx, rows)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]bool, len(rows))
	for name := range installed {
		remaining[name] = true
	}
	delete(remaining, fullName)

	if by := requiredBy(fullName, remaining, needs); len(by) > 0 {
		return nil, &requiredByError{FullName: fullName, By: by}
	}
	names := []string{fullName}
	if removeOrphans {
		names = append(names, orphansOf(installed, remaining, needs)...)
	}
	return names, nil
}

// dependencyEdges maps each installed package to the full names it depends on, read from
// the cached index at the version that is actually installed.
//
// `↯` A version the index no longer carries contributes no edges. That is the honest
// answer — the panel does not know what it needed — and it is the same source the resolver
// used to install it; the alternative, refusing every uninstall until the next sync, would
// make one stale row block the whole feature.
func (m *Mods) dependencyEdges(ctx context.Context, rows []store.InstanceMod) (map[string][]string, error) {
	needs := make(map[string][]string, len(rows))
	for i := range rows {
		deps, ok, err := m.DB.ModVersionDependencies(ctx, rows[i].FullName, rows[i].Version)
		if err != nil {
			return nil, fmt.Errorf("read the dependencies of %s-%s: %w",
				rows[i].FullName, rows[i].Version, err)
		}
		if !ok {
			continue
		}
		for _, dep := range deps {
			depName, _, ok := modresolver.ParseDependency(dep)
			if !ok {
				continue
			}
			needs[rows[i].FullName] = append(needs[rows[i].FullName], depName)
		}
	}
	return needs, nil
}

// requiredBy names the packages still installed that depend on fullName.
func requiredBy(fullName string, remaining map[string]bool, needs map[string][]string) []string {
	var by []string
	for name := range remaining {
		for _, dep := range needs[name] {
			if dep == fullName {
				by = append(by, name)
				break
			}
		}
	}
	sort.Strings(by)
	return by
}

// orphansOf is every remaining `dependency` row that nothing remaining needs, to a fixed
// point — removing one orphan can orphan the package it in turn pulled in. It mutates
// remaining as it goes, so each pass sees the set as it would be after the removals already
// decided on.
func orphansOf(
	installed map[string]*store.InstanceMod, remaining map[string]bool, needs map[string][]string,
) []string {
	var orphans []string
	for {
		var found []string
		for name := range remaining {
			if installed[name].InstalledAs != store.InstalledDependency {
				continue
			}
			if len(requiredBy(name, remaining, needs)) == 0 {
				found = append(found, name)
			}
		}
		if len(found) == 0 {
			sort.Strings(orphans)
			return orphans
		}
		for _, name := range found {
			delete(remaining, name)
		}
		orphans = append(orphans, found...)
	}
}

// writeRemovalError maps the two answers an uninstall request can be refused with onto the
// registry: a mod that is not there, and one another mod needs.
func writeRemovalError(w http.ResponseWriter, r *http.Request, err error) {
	var notInstalled *notInstalledError
	if errors.As(err, &notInstalled) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	var required *requiredByError
	if errors.As(err, &required) {
		apierr.Write(w, r, apierr.New(apierr.ModConflict).
			With("required_by", required.By).
			Wrap(err))
		return
	}
	apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
}

// modUninstallCancelPolicy is 12 §3.1's row: not cancellable, at any point. The job is
// seconds of file removal, and the only thing an interruption could deliver is half a
// package removed — which is worse than either outcome the user was choosing between.
func modUninstallCancelPolicy(string) (cancellable bool, phase string) {
	return false, "removing files from the server directory"
}

// removedPackage is one package of the removal set, with the manifest that defines it.
type removedPackage struct {
	fullName string
	manifest []installer.ManifestEntry
}

// runModUninstall is the mod_uninstall Runner: save every file the manifests name, remove
// them, and only then — in the job's own Finish transaction — delete the rows. The order is
// what makes the crash cases benign: while the files are gone the rows still describe them,
// and the backups are still on disk to put them back.
func (m *Mods) runModUninstall(inst *store.Instance, payload modUninstallPayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()

		pkgs, err := m.removalManifests(ctx, inst.ID, payload.FullNames)
		if err != nil {
			return modJobFailed(apierr.Internal, err)
		}
		serverRoot := serverDir(inst)
		backupDir := stagingBackupDir(payload.StagingDir)

		h.Progress(ctx, 20, fmt.Sprintf("saving the files of %d packages", len(pkgs)))
		for _, p := range pkgs {
			if err := installer.BackupPaths(
				installer.Paths(p.manifest), serverRoot, backupDir); err != nil {
				// Nothing has been removed, so there is nothing to put back.
				return modJobFailed(apierr.Internal, fmt.Errorf("save %s: %w", p.fullName, err))
			}
		}
		if err := h.Checkpoint(ctx, checkpointSaved); err != nil {
			return modJobFailed(apierr.Internal, err)
		}

		h.Progress(ctx, 60, "removing files")
		for _, p := range pkgs {
			if err := installer.Remove(installer.Paths(p.manifest), serverRoot); err != nil {
				return m.rollbackUninstall(ctx, inst, pkgs, backupDir,
					fmt.Errorf("remove %s: %w", p.fullName, err))
			}
			h.Log(fmt.Sprintf("%s: %d files removed", p.fullName, len(p.manifest)))
		}
		if err := h.Checkpoint(ctx, checkpointRemoved); err != nil {
			return m.rollbackUninstall(ctx, inst, pkgs, backupDir, err)
		}

		h.Progress(ctx, 100, fmt.Sprintf("removed %d packages", len(pkgs)))
		return jobs.Outcome{
			Status:   "succeeded",
			OnFinish: finishUninstall(inst.ID, payload.FullNames),
		}
	}
}

// removalManifests reads the manifest of every package in the removal set. A row that has
// gone missing since the request stops the job: this is the only exact record of that
// package's files, and removing a package whose manifest is unreadable is the heuristic
// re-run B9 forbids.
func (m *Mods) removalManifests(
	ctx context.Context, instanceID string, fullNames []string,
) ([]removedPackage, error) {
	rows, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	byName := make(map[string]string, len(rows))
	for i := range rows {
		byName[rows[i].FullName] = rows[i].FileManifest
	}

	pkgs := make([]removedPackage, 0, len(fullNames))
	for _, name := range fullNames {
		raw, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("%s is no longer installed", name)
		}
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
			return nil, fmt.Errorf("read the manifest of %s: %w", name, err)
		}
		pkgs = append(pkgs, removedPackage{fullName: name, manifest: manifest})
	}
	return pkgs, nil
}

// rollbackUninstall puts back everything the job saved. Every package is attempted even
// after one fails, and what could not be restored is named — an uninstall that failed is
// ordinary, one that left the server in neither state is not.
func (m *Mods) rollbackUninstall(
	ctx context.Context, inst *store.Instance, pkgs []removedPackage, backupDir string, cause error,
) jobs.Outcome {
	serverRoot := serverDir(inst)
	var stuck []string
	for _, p := range pkgs {
		if err := installer.Rollback(p.manifest, serverRoot, backupDir); err != nil {
			slog.ErrorContext(ctx, "mod uninstall rollback incomplete",
				slog.String("instance_id", inst.ID), slog.String("full_name", p.fullName),
				slog.Any("error", err))
			stuck = append(stuck, p.fullName)
		}
	}
	if len(stuck) > 0 {
		return modJobFailed(apierr.Internal,
			fmt.Errorf("%w; and these could not be put back: %s", cause, strings.Join(stuck, ", ")))
	}
	return modJobFailed(apierr.Internal, cause)
}

// finishUninstall is the state flip (12 §6): the rows go, the instance is marked as needing
// a restart, and an instance that has just lost BepInEx stops being a modded one.
func finishUninstall(instanceID string, fullNames []string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxDeleteInstanceMods(ctx, tx, instanceID, fullNames); err != nil {
			return fmt.Errorf("remove the rows of an uninstall: %w", err)
		}
		if err := store.TxSetRestartRequired(ctx, tx, instanceID); err != nil {
			return fmt.Errorf("mark %s as needing a restart: %w", instanceID, err)
		}
		for _, name := range fullNames {
			if name == BepInExPack {
				return store.TxClearModded(ctx, tx, instanceID)
			}
		}
		return nil
	}
}

// modPatchRequest is PATCH /instances/{id}/mods/{full_name}'s body. Both fields are
// optional and a nil one is left alone, so a client that knows about one field cannot blank
// the other by omitting it.
type modPatchRequest struct {
	Side    *string `json:"side"`
	Enabled *bool   `json:"enabled"`
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
	if body.Side == nil && body.Enabled == nil {
		val.Add("side", apierr.FieldRequired, "Give at least one of side or enabled.")
	}
	if body.Side != nil && !sides[*body.Side] {
		val.Add("side", apierr.FieldInvalid,
			"side is one of server_only, client_required, client_optional, unknown.")
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
// `↯` `side` is set here and nowhere else. 03 §5.6 says Thunderstore metadata does not
// reliably encode whether a mod is needed on the client, and a panel that guessed would
// produce a client manifest that silently omits a required mod — which presents to players
// as an unexplained failure to connect.
//
// `◇` `enabled` is recorded and reported, and nothing on disk changes: what disabling a mod
// without uninstalling it should *do* is not settled anywhere in the pack (Q37). Until it
// is, this is a label like `side`, and the UI must not offer it as a working switch.
func (m *Mods) patchMod(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	body, ok := decodeModPatch(w, r)
	if !ok {
		return
	}

	fullName := r.PathValue("full_name")
	found, err := m.DB.SetInstanceModTags(r.Context(), id, fullName, body.Side, body.Enabled)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if !found {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	mods, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	for i := range mods {
		if mods[i].FullName == fullName {
			// No load status on a PATCH response: it changes no file, so re-reading
			// BepInEx's log to answer a tag edit would be work for an answer nobody asked
			// this endpoint for. GET /instances/{id}/mods is where that lives.
			pkg, err := m.DB.ModPackageByFullName(r.Context(), fullName)
			if err != nil {
				apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
				return
			}
			JSON(w, r, http.StatusOK, toInstalledModView(&mods[i], pkg, nil))
			return
		}
	}
	apierr.Write(w, r, apierr.New(apierr.NotFound))
}
