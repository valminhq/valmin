package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// "Update all mods" (Q39). Every installed package with a newer version in the registry its
// files came from moves to that version in one mod_install job: one resolve, one combined diff
// the operator confirms, one commit, and one rollback if any of it fails. A world archive is
// taken before the first file changes, because a mod update is the change most likely to leave a
// world the new versions cannot read.

// updateTarget is one installed package and the version an update moves it to. The preview
// hands the list back and the apply request returns it, so the job installs what the operator
// confirmed rather than whatever a sync in between made newest.
type updateTarget struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	// FromVersion is the installed version, for the preview. The apply request may omit it.
	FromVersion string `json:"from_version,omitempty"`
	Version     string `json:"version"`
}

// updateNode is one row of the combined diff: a package whose files the update changes.
type updateNode struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	// FromVersion is empty for a package the updates newly pull in as a dependency.
	FromVersion string `json:"from_version"`
	Version     string `json:"version"`
	Transitive  bool   `json:"transitive"`
}

type updatePreview struct {
	Targets []updateTarget `json:"targets"`
	Nodes   []updateNode   `json:"nodes"`
	// Backup reports whether an archive will be taken first. False only for an instance with no
	// world yet, which has nothing to lose.
	Backup bool `json:"backup"`
}

type applyUpdatesRequest struct {
	Targets []updateTarget `json:"targets"`
}

// pendingUpdates is every installed package the catalogue has a newer version of, from the
// registry its files came from and only while that registry is enabled: the same rule the
// installed list's update_version follows, so the button and the badges cannot disagree.
func (m *Mods) pendingUpdates(ctx context.Context, instanceID string) ([]updateTarget, error) {
	rows, err := m.DB.InstanceModsCatalogued(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	out := []updateTarget{}
	for i := range rows {
		// A disabled mod is left where it is: its files are parked, and the operator who parked
		// it is hunting a problem an update would change underneath them (Q37).
		if !rows[i].Enabled || !m.sourceEnabled(rows[i].Source) {
			continue
		}
		version := modUpdateVersion(&rows[i].InstanceMod, rows[i].Package)
		if version == "" {
			continue
		}
		out = append(out, updateTarget{
			FullName: rows[i].FullName, Source: rows[i].Source.String(),
			FromVersion: rows[i].Version, Version: version,
		})
	}
	return out, nil
}

// resolveUpdateClosure resolves every target at once, so a dependency two updates share is
// raised once to the higher of their demands (03 §6.3), not installed twice in two jobs.
func resolveUpdateClosure(targets []updateTarget, idx *storeIndex) (modresolver.Closure, error) {
	requests := make([]modresolver.Request, 0, len(targets))
	for _, t := range targets {
		requests = append(requests, modresolver.Request{FullName: t.FullName, Version: t.Version})
	}
	closure, err := modresolver.Resolve(requests, idx)
	if idx.err != nil {
		// As in resolveClosure: a store read failed, so the verdict is worthless and the caller
		// reports idx.err instead.
		return closure, nil //nolint:nilerr // idx.err is the real failure, and the caller reads it
	}
	if err != nil {
		return closure, fmt.Errorf("resolve %d updates: %w", len(targets), err)
	}
	return closure, nil
}

// previewUpdates is POST /instances/{id}/mods/updates/resolve: the combined diff "Update all"
// would apply, computed and discarded, gated as the single-package resolve is.
func (m *Mods) previewUpdates(w http.ResponseWriter, r *http.Request) {
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

	targets, err := m.pendingUpdates(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	preview := updatePreview{Targets: targets, Nodes: []updateNode{}, Backup: hasWorlds(inst)}
	if len(targets) == 0 {
		JSON(w, r, http.StatusOK, preview)
		return
	}

	idx := m.newStoreIndex(r.Context(), id, source.Source{})
	closure, resolveErr := resolveUpdateClosure(targets, idx)
	if idx.err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(idx.err))
		return
	}
	if resolveErr != nil {
		writeResolveError(w, r, resolveErr)
		return
	}
	installed, err := m.installedVersions(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	for _, n := range closure.Nodes {
		if n.NoOp {
			continue
		}
		preview.Nodes = append(preview.Nodes, updateNode{
			FullName: n.FullName, Source: idx.sourceOf(n.FullName, n.Version).String(),
			FromVersion: installed[n.FullName], Version: n.Version, Transitive: n.Transitive,
		})
	}
	JSON(w, r, http.StatusOK, preview)
}

// applyUpdates is POST /instances/{id}/mods/updates: submit the confirmed targets as one
// mod_install job that archives the world first. It answers 202 with the job.
func (m *Mods) applyUpdates(w http.ResponseWriter, r *http.Request) {
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
	inst, ok := m.mustLoadEditableInstance(w, r, id)
	if !ok {
		return
	}
	var body applyUpdatesRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var val apierr.Validation
	targets, err := m.checkUpdateTargets(r.Context(), id, body.Targets, &val)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	job, err := m.submitPayload(r.Context(), inst, &modInstallPayload{
		Updates: targets, Backup: true,
	}, "update", u.ID, nil)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// checkUpdateTargets validates the confirmed list against what is installed now. Each target
// must be an installed package, from the registry its files came from, while that registry is
// enabled, moving to a version above the installed one: an update never re-sources a package
// (B14) and never downgrades one. Refusals go into val; the error is a store failure.
func (m *Mods) checkUpdateTargets(
	ctx context.Context, instanceID string, targets []updateTarget, val *apierr.Validation,
) ([]updateTarget, error) {
	if len(targets) == 0 {
		val.Add("targets", apierr.FieldRequired, "Name at least one mod to update.")
		return nil, nil
	}
	rows, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	byName := make(map[string]*store.InstanceMod, len(rows))
	for i := range rows {
		byName[rows[i].FullName] = &rows[i]
	}

	seen := map[string]bool{}
	out := make([]updateTarget, 0, len(targets))
	for i, t := range targets {
		field := fmt.Sprintf("targets[%d]", i)
		row := byName[t.FullName]
		switch {
		case seen[t.FullName]:
			val.Add(field, apierr.FieldInvalid, t.FullName+" is named twice.")
		case row == nil:
			val.Add(field, apierr.FieldInvalid, t.FullName+" is not installed on this server.")
		case !row.Enabled:
			val.Add(field, apierr.FieldInvalid, t.FullName+" is disabled. Enable it before updating it.")
		case t.Source != row.Source.String():
			val.Add(field, apierr.FieldInvalid,
				t.FullName+" was installed from "+row.Source.String()+" and updates from there only.")
		case !m.sourceEnabled(row.Source):
			val.Add(field, apierr.FieldInvalid, row.Source.String()+" is not enabled on this panel.")
		case !newer(t.Version, row.Version):
			val.Add(field, apierr.FieldInvalid,
				t.Version+" is not newer than the installed "+row.Version+".")
		default:
			out = append(out, updateTarget{
				FullName: t.FullName, Source: t.Source, FromVersion: row.Version, Version: t.Version,
			})
		}
		seen[t.FullName] = true
	}
	return out, nil
}

func (m *Mods) sourceEnabled(src source.Source) bool {
	_, ok := m.Clients[src]
	return ok
}

// newer reports whether candidate parses and is above installed. An installed version that does
// not parse cannot be compared, and an update over it is refused rather than guessed at.
func newer(candidate, installed string) bool {
	c, cOK := semver.ParseVersion(candidate)
	i, iOK := semver.ParseVersion(installed)
	return cOK && iOK && semver.Compare(c, i) > 0
}

// installedVersions maps each installed package to its version, for the diff's "from" column.
func (m *Mods) installedVersions(ctx context.Context, instanceID string) (map[string]string, error) {
	rows, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].FullName] = rows[i].Version
	}
	return out, nil
}

// hasWorlds reports whether an archive would have anything to hold, the same test
// snapshotWorlds makes before it archives.
func hasWorlds(inst *store.Instance) bool {
	_, err := os.Stat(instance.WorldsDir(inst.DataDir)) //nolint:gosec // data_dir is panel-generated
	return err == nil
}

// archiveBeforeModUpdate takes the world archive an update promises, before any file moves. A
// nil record with a nil error is an instance with no world yet.
func (m *Mods) archiveBeforeModUpdate(
	ctx context.Context, h *jobs.Handle, inst *store.Instance,
) (func(context.Context, *sql.Tx) error, error) {
	if m.ArchiveWorlds == nil {
		return nil, errors.New("this panel cannot archive worlds, so it will not update mods without a backup")
	}
	h.Progress(ctx, 64, "backing up the world")
	record, err := m.ArchiveWorlds(inst, store.TriggerPreUpdate)
	if err != nil {
		return nil, fmt.Errorf("back up the world before updating mods: %w", err)
	}
	if record == nil {
		h.Log("no world archive was taken: this server has no world yet")
	}
	return record, nil
}

// withArchive records the archive in the job's own Finish transaction whatever the outcome. It
// is the world the operator had, and a failed update is when they are most likely to want it.
func withArchive(out jobs.Outcome, archived func(context.Context, *sql.Tx) error) jobs.Outcome {
	if archived == nil {
		return out
	}
	then := out.OnFinish
	out.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
		if err := archived(ctx, tx); err != nil {
			return err
		}
		if then == nil {
			return nil
		}
		return then(ctx, tx)
	}
	return out
}

// updateSummary names what an update job was asked for, for its log.
func updateSummary(targets []updateTarget) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s %s -> %s", t.FullName, t.FromVersion, t.Version))
	}
	return "updating " + strings.Join(parts, ", ")
}
