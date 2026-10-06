package api

import (
	"context"
	"fmt"
	"net/http"
	"os"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// "Update all mods" (Q39). Every installed package with a newer version in the registry its
// files came from, except a locked one, a modpack and a member following its modpack, moves to
// that version in one mod_install job: one resolve, one combined diff
// the operator confirms, one commit, and one rollback if any of it fails. A world archive is
// taken before the first file changes, because a mod update is the change most likely to leave a
// world the new versions cannot read.

// updateTarget is one installed package and the version an update moves it to. The preview
// hands the list back and the apply request returns it, so the job installs what the operator
// confirmed rather than whatever a sync in between made newest.
type updateTarget = manager.UpdateTarget

// updateNode is one row of the combined diff: a package whose files the update changes.
type updateNode struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	// FromVersion is empty for a package the updates newly pull in as a dependency.
	FromVersion string `json:"from_version"`
	Version     string `json:"version"`
	// Change is install, upgrade or downgrade.
	Change     string `json:"change"`
	Transitive bool   `json:"transitive"`
}

type updatePreview struct {
	Targets []updateTarget `json:"targets"`
	Nodes   []updateNode   `json:"nodes"`
	// Conflicts are dependencies the updates would leave unmet, such as a locked package another
	// update needs raised. The apply refuses while any remain.
	Conflicts []conflictView `json:"conflicts"`
	// Backup reports whether an archive will be taken first. False only for an instance with no
	// world yet, which has nothing to lose.
	Backup bool `json:"backup"`
}

type applyUpdatesRequest struct {
	Targets []updateTarget `json:"targets"`
}

// pendingUpdates delegates update selection to the mod manager.
func (m *Mods) pendingUpdates(ctx context.Context, instanceID string) ([]updateTarget, error) {
	return m.planner().PendingUpdates(ctx, instanceID) //nolint:wrapcheck // preserve catalogue read errors
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
	preview := updatePreview{
		Targets: targets, Nodes: []updateNode{}, Conflicts: []conflictView{}, Backup: hasWorlds(inst),
	}
	if len(targets) == 0 {
		JSON(w, r, http.StatusOK, preview)
		return
	}

	idx := m.newStoreIndex(r.Context(), id, source.Source{})
	plan, resolveErr := manager.PlanUpdates(targets, idx)
	if idx.Err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(idx.Err))
		return
	}
	if resolveErr != nil {
		writeResolveError(w, r, resolveErr)
		return
	}
	if off := manager.DisabledInClosure(manager.ClosureNames(plan.Closure), idx.Rows()); len(off) > 0 {
		writeDisabledConflict(w, r, off)
		return
	}
	for _, n := range plan.Closure.Nodes {
		if n.NoOp {
			continue
		}
		from := idx.Have[n.FullName].Version
		preview.Nodes = append(preview.Nodes, updateNode{
			FullName: n.FullName, Source: idx.SourceOf(n.FullName, n.Version).String(),
			FromVersion: from, Version: n.Version, Change: changeOf(from, n.Version, false),
			Transitive: n.Transitive,
		})
	}
	preview.Conflicts = toConflictViews(plan.Conflicts, idx)
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

	packages := make([]modVersionChange, len(targets))
	for i, t := range targets {
		packages[i] = modVersionChange{FullName: t.FullName, From: t.FromVersion, To: t.Version}
	}
	audit := jobAudit(r.Context(), u.ID, id, "instances.mods.update", map[string]any{"packages": packages})
	job, err := m.installer().Submit(r.Context(), inst, &manager.InstallPayload{
		Updates: targets, Backup: true,
	}, "update", u.ID, audit, nil)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// checkUpdateTargets maps manager validation issues to the HTTP field error envelope.
func (m *Mods) checkUpdateTargets(
	ctx context.Context, instanceID string, targets []updateTarget, val *apierr.Validation,
) ([]updateTarget, error) {
	checked, issues, err := m.planner().CheckUpdateTargets(ctx, instanceID, targets)
	if err != nil {
		return nil, fmt.Errorf("check mod updates: %w", err)
	}
	for _, issue := range issues {
		code := apierr.FieldInvalid
		if issue.Field == "targets" {
			code = apierr.FieldRequired
		}
		val.Add(issue.Field, code, issue.Message)
	}
	return checked, nil
}

// hasWorlds reports whether an archive would have anything to hold, the same test
// snapshotWorlds makes before it archives.
func hasWorlds(inst *store.Instance) bool {
	_, err := os.Stat(instance.WorldsDir(inst.DataDir)) //nolint:gosec // data_dir is panel-generated
	return err == nil
}
