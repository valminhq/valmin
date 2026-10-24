package api

import (
	"errors"
	"net/http"
	"slices"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/profile"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// Applying a profile code to a server that exists. The code's mods are installed, raised or
// lowered in one mod_install job, from the registry each already lives in or, for a new mod,
// the one the code names or was served by. An installed mod never changes registry.

// codePreviewRequest is the body of the preview. Select, when present, names exactly the packages
// to apply; absent, installs and raised versions are chosen and lowered ones are not.
type codePreviewRequest struct {
	Code   string    `json:"code"`
	Select *[]string `json:"select"`
}

// codeRowView is one package of a code, and whether the preview applies it.
type codeRowView struct {
	manager.CodeRow
	Selected bool `json:"selected"`
}

// codePreview is what applying a code would do. Targets are the chosen changes and are sent back
// to apply; Nodes are every package whose files change, dependencies included. Problems stop the
// apply: a selection that does not resolve, a disabled mod in the way, or a missing framework.
type codePreview struct {
	ProfileName string                 `json:"profile_name"`
	Registry    string                 `json:"registry"`
	Matching    int                    `json:"matching"`
	Rows        []codeRowView          `json:"rows"`
	Targets     []manager.UpdateTarget `json:"targets"`
	Nodes       []updateNode           `json:"nodes"`
	Conflicts   []conflictView         `json:"conflicts"`
	Problems    []string               `json:"problems"`
	Backup      bool                   `json:"backup"`
}

// noFrameworkText is why a code cannot be applied to a server that has no mod framework: the
// framework arrives, at its newest version, with the first mod installed from Browse mods.
const noFrameworkText = "This server has no mod framework yet. Install any mod from Browse mods first, " +
	"then apply the code."

// applyCodeRequest confirms a preview. Registry is the one the code was served by, which the
// job prefers for dependencies the code does not list.
type applyCodeRequest struct {
	Targets  []manager.UpdateTarget `json:"targets"`
	Registry string                 `json:"registry"`
	Unowned  installer.Unowned      `json:"unowned,omitempty"`
}

// previewCode is POST /instances/{id}/mods/code/preview: the changes a profile code would make,
// computed and discarded.
func (m *Mods) previewCode(w http.ResponseWriter, r *http.Request) {
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
	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body codePreviewRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	preview, err := m.buildCodePreview(r, inst, &body)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	JSON(w, r, http.StatusOK, preview)
}

// buildCodePreview reads the profile a code names and works out what applying it to inst would
// do. The error is a response.
func (m *Mods) buildCodePreview(r *http.Request, inst *store.Instance, body *codePreviewRequest) (*codePreview, error) {
	p, origin, err := m.readProfileCode(r, body.Code)
	if err != nil {
		return nil, err
	}
	mods := make([]manager.CodeMod, 0, len(p.Mods))
	for _, mod := range p.Mods {
		if mod.Enabled {
			mods = append(mods, manager.CodeMod{FullName: mod.FullName, Version: mod.Version, Source: mod.Source})
		}
	}
	rows, matching, err := m.plan.ClassifyCode(r.Context(), inst.ID, mods, origin)
	if err != nil {
		return nil, apierr.New(errcode.Internal).Wrap(err)
	}
	selected := manager.DefaultCodeSelection
	if body.Select != nil {
		chosen := *body.Select
		selected = func(row *manager.CodeRow) bool { return slices.Contains(chosen, row.FullName) }
	}

	preview := &codePreview{
		ProfileName: p.Name, Registry: origin.String(), Matching: matching,
		Rows:    make([]codeRowView, 0, len(rows)),
		Targets: manager.CodeTargets(rows, selected),
		Nodes:   []updateNode{}, Conflicts: []conflictView{}, Problems: []string{},
		Backup: hasWorlds(inst),
	}
	for i := range rows {
		preview.Rows = append(
			preview.Rows,
			codeRowView{CodeRow: rows[i], Selected: rows[i].Actionable() && selected(&rows[i])},
		)
	}
	if len(preview.Targets) == 0 {
		return preview, nil
	}
	if !inst.Modded {
		preview.Problems = append(preview.Problems, noFrameworkText)
		return preview, nil
	}
	idx := m.newStoreIndex(r.Context(), inst.ID, origin)
	plan, resolveErr := manager.PlanUpdates(preview.Targets, idx)
	if idx.Err() != nil {
		return nil, apierr.New(errcode.Internal).Wrap(idx.Err())
	}
	if resolveErr != nil {
		preview.Problems = append(preview.Problems, resolveProblem(resolveErr))
		return preview, nil
	}
	describeCodePlan(preview, &plan, idx)
	return preview, nil
}

// describeCodePlan fills the closure, conflicts and problems of a plan that resolved.
func describeCodePlan(p *codePreview, plan *manager.ChangePlan, idx *manager.Index) {
	names := manager.ClosureNames(plan.Closure)
	for _, off := range manager.DisabledInClosure(names, idx.Rows()) {
		p.Problems = append(p.Problems, off+" is disabled. Enable it before applying these changes.")
	}
	for _, n := range plan.Closure.Nodes {
		if n.NoOp {
			continue
		}
		from, _ := idx.Installed(n.FullName)
		p.Nodes = append(p.Nodes, updateNode{
			FullName: n.FullName, Source: idx.SourceOf(n.FullName, n.Version).String(),
			FromVersion: from, Version: n.Version, Change: changeOf(from, n.Version, false),
			Transitive: n.Transitive,
		})
	}
	p.Conflicts = toConflictViews(plan.Conflicts, idx)
}

// resolveProblem words a resolver failure for the preview.
func resolveProblem(err error) string {
	if unresolved, ok := errors.AsType[*modresolver.UnresolvedError](err); ok {
		return unresolved.Ident() + " is not available in the mod registries."
	}
	if held, ok := errors.AsType[*modresolver.HeldError](err); ok {
		return held.FullName + " is locked and cannot move."
	}
	return "These changes do not resolve together."
}

// readProfileCode downloads and reads the profile a pasted code names, and returns the registry
// that served it. The error is a response: a rejected code, or an unavailable registry.
func (m *Mods) readProfileCode(r *http.Request, code string) (*profile.Profile, source.Source, error) {
	key, ok := profile.ParseKey(code)
	if !ok {
		return nil, source.Source{}, codeInvalid("Paste a profile code from r2modman or Gale.")
	}
	archive, origin, err := fetchProfile(r.Context(), m.Profiles, key)
	if err != nil {
		return nil, source.Source{}, err
	}
	p, err := profile.Read(archive, profile.Limits{
		ManifestBytes: maxProfileManifestBytes, Configs: maxManifestConfigs,
		ConfigBytes: control.MaxConfigSize, TotalBytes: maxManifestBytes,
	})
	if err != nil {
		return nil, source.Source{}, codeInvalid("This profile could not be read: " + err.Error() + ".")
	}
	if len(p.Mods) > maxManifestMods {
		return nil, source.Source{}, codeInvalid("This profile lists more mods than this panel applies at once.")
	}
	return p, origin, nil
}

// applyCode is POST /instances/{id}/mods/code/apply: submit the confirmed changes as one
// mod_install job that archives the world first. It answers 202 with the job.
func (m *Mods) applyCode(w http.ResponseWriter, r *http.Request) {
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
	var body applyCodeRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	var val apierr.Validation
	if !inst.Modded {
		val.Add("targets", apierr.FieldInvalid, noFrameworkText)
	}
	if !body.Unowned.Valid() {
		val.Add("unowned", apierr.FieldNotAnOption, `unowned must be "keep" or "replace".`)
	}
	if _, known := source.ByName(body.Registry); body.Registry != "" && !known {
		val.Add("registry", apierr.FieldInvalid, "registry must name a configured mod registry.")
	}
	targets, issues, err := m.plan.CheckCodeTargets(r.Context(), id, body.Targets)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	for _, issue := range issues {
		code := apierr.FieldInvalid
		if issue.Field == "targets" {
			code = apierr.FieldRequired
		}
		val.Add(issue.Field, code, issue.Message)
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}
	m.submitUpdates(w, r, u.ID, inst, targets, body.Unowned, body.Registry)
}
