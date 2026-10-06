package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// manifestSchema is the only version this build writes and the only one it reads. An unknown
// value is refused rather than best-effort parsed (ADR-151).
const manifestSchema = 1

// The manifest's bounds, enforced on the way in. An importer reads untrusted bytes, so every
// dimension it could grow along has a ceiling (ADR-151).
const (
	// maxManifestBytes is the whole document's cap, applied by the handler because 11 §8.3's
	// 1 MiB JSON limit is exempted for these two routes — a real modpack's config is bigger
	// than that, and a manifest missing config is not a definition (ADR-151).
	maxManifestBytes   = 8 << 20
	maxManifestMods    = 200
	maxManifestConfigs = 200
)

// manifestMod is one pinned package. The side tag travels because it is the admin's own
// classification (03 §5.6) and re-tagging a restored server by hand is work nobody recorded.
// Source is the registry the files came from, since two registries can publish different bytes
// under one name and version; a manifest without it installs from whichever carries the version.
type manifestMod struct {
	FullName string `json:"full_name"`
	Source   string `json:"source,omitempty"`
	Version  string `json:"version"`
	Side     string `json:"side,omitempty"`
}

type instanceManifest struct {
	Schema   int                      `json:"schema"`
	Name     string                   `json:"name"`
	Instance control.ManifestLaunch   `json:"instance"`
	Mods     []manifestMod            `json:"mods"`
	Configs  []control.ManifestConfig `json:"configs"`
}

// importRequest is POST /instances/import. Name and password come from the caller, never from
// the file: the manifest carries no password, and a name has to be free on this panel.
type importRequest struct {
	Manifest            *instanceManifest `json:"manifest"`
	Name                string            `json:"name"`
	Password            string            `json:"password"`
	StartAfterProvision bool              `json:"start_after_provision,omitempty"`
}

// manifestPreview is what an import would do, reported without writing anything.
type manifestPreview struct {
	Name     string                 `json:"name"`
	Instance control.ManifestLaunch `json:"instance"`
	Mods     []previewModView       `json:"mods"`
	Configs  []previewFileView      `json:"configs"`
	Problems []manifestProblem      `json:"problems"`
}

type previewModView struct {
	FullName string `json:"full_name"`
	Source   string `json:"source,omitempty"`
	Version  string `json:"version"`
	Side     string `json:"side,omitempty"`
	// Available is false when this exact version is no longer in the catalogue. The import
	// refuses rather than resolving to a nearby version (ADR-151).
	Available bool `json:"available"`
}

type previewFileView struct {
	File  string `json:"file"`
	Bytes int    `json:"size_bytes"`
}

// manifestProblem is one reason an import would be refused, named so the screen can say which
// part of the file is wrong rather than that the file is wrong.
type manifestProblem struct {
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// exportManifest is GET /instances/{id}/manifest (04 §3): 01 G6's definition of one instance.
// It projects three things the caller can already read one endpoint at a time, so it checks
// all three here — authorization is never middleware (ADR-037).
func (h *Instances) exportManifest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	for _, action := range []authz.Action{authz.InstanceSettings, authz.ModsList, authz.ConfigRead} {
		if !h.Authz.Can(r.Context(), u, action, id) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}

	manifest, _, err := h.instanceDefinition(r.Context(), inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, manifest)
}

// instanceDefinition is the single read path for G6's reproducible definition. Export returns
// its document; clone consumes the same snapshot while also retaining the richer installed rows
// needed to preserve file manifests and explicit/dependency provenance.
func (h *Instances) instanceDefinition(
	ctx context.Context, inst *store.Instance,
) (*instanceManifest, []store.InstanceMod, error) {
	installed, err := h.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("read installed mods for instance %s: %w", inst.ID, err)
	}
	configs, err := readInstanceConfigs(inst)
	if err != nil {
		return nil, nil, err
	}
	manifest := &instanceManifest{
		Schema:   manifestSchema,
		Name:     inst.Name,
		Instance: control.LaunchOf(inst),
		Mods:     make([]manifestMod, 0, len(installed)),
		Configs:  configs,
	}
	for i := range installed {
		manifest.Mods = append(manifest.Mods, manifestMod{
			FullName: installed[i].FullName, Source: installed[i].Source.String(),
			Version: installed[i].Version, Side: installed[i].Side,
		})
	}
	return manifest, installed, nil
}

// readInstanceConfigs reads every portable .cfg in the instance's config directory whole. A
// server that has never started has none, which is an empty list rather than an error (03 §9).
func readInstanceConfigs(inst *store.Instance) ([]control.ManifestConfig, error) {
	dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(instance.ConfigDir))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []control.ManifestConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config directory: %w", err)
	}
	out := []control.ManifestConfig{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cfg") {
			continue
		}
		//nolint:gosec // dir is the instance's own config directory and e.Name() came from it
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", e.Name(), err)
		}
		out = append(out, control.ManifestConfig{File: e.Name(), Content: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return portableConfigs(out), nil
}

// portableConfigs drops the config files that belong to one installation rather than to its
// definition. The RCON plugin's file holds the password the panel generates for each instance,
// so exporting it would leak that secret and importing it would give the new server another
// server's password.
func portableConfigs(configs []control.ManifestConfig) []control.ManifestConfig {
	return slices.DeleteFunc(configs, func(c control.ManifestConfig) bool { return c.File == command.ConfigFile })
}

// previewManifest is POST /instances/manifest/preview (04 §3): the same validation the import
// runs, reported instead of applied. Gated on instance.create, since it is the first half of
// a create and reports the file's contents back.
func (h *Instances) previewManifest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestBytes)
	var body importRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	manifest := body.Manifest
	if manifest == nil {
		var val apierr.Validation
		val.Add("manifest", apierr.FieldRequired, "A manifest is required.")
		apierr.Write(w, r, val.Err())
		return
	}
	manifest.Configs = portableConfigs(manifest.Configs)

	preview := manifestPreview{
		Name:     manifest.Name,
		Instance: manifest.Instance,
		Mods:     []previewModView{},
		Configs:  []previewFileView{},
		Problems: validateManifest(manifest),
	}
	for _, mod := range manifest.Mods {
		// A mod naming its registry must be available there. One naming none may come from any.
		allowed := source.All()
		if src, ok := source.ByName(mod.Source); ok {
			allowed = []source.Source{src}
		}
		_, _, available, err := h.DB.ModVersionDependenciesFrom(
			r.Context(), mod.FullName, mod.Version, source.Source{}, allowed)
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		preview.Mods = append(preview.Mods, previewModView{
			FullName: mod.FullName, Source: mod.Source, Version: mod.Version, Side: mod.Side,
			Available: available,
		})
		if !available {
			where := "the catalogue"
			if mod.Source != "" {
				where = mod.Source
			}
			preview.Problems = append(preview.Problems, manifestProblem{
				Field:  "mods",
				Detail: mod.FullName + " " + mod.Version + " is not in " + where + ".",
			})
		}
	}
	for _, cfg := range manifest.Configs {
		preview.Configs = append(preview.Configs, previewFileView{File: cfg.File, Bytes: len(cfg.Content)})
	}
	JSON(w, r, http.StatusOK, preview)
}

// importManifest is POST /instances/import (04 §3). It refuses a manifest it cannot apply
// before anything is written, then hands the rest to the create path: an import and a wizard
// create provision, install and recover identically (ADR-151, ADR-116).
func (h *Instances) importManifest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceCreate, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestBytes)
	var body importRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	var val apierr.Validation
	if body.Manifest == nil {
		val.Add("manifest", apierr.FieldRequired, "A manifest is required.")
		apierr.Write(w, r, val.Err())
		return
	}
	body.Manifest.Configs = portableConfigs(body.Manifest.Configs)
	for _, problem := range validateManifest(body.Manifest) {
		val.Add(problem.Field, apierr.FieldInvalid, problem.Detail)
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

	manifest := body.Manifest
	name := body.Name
	if name == "" {
		name = manifest.Name
	}
	create := &createInstanceRequest{
		Name: name, ServerName: manifest.Instance.ServerName, WorldName: manifest.Instance.WorldName,
		Password: body.Password, Public: manifest.Instance.Public,
		Crossplay: manifest.Instance.Crossplay, Preset: manifest.Instance.Preset,
		Modifiers: manifest.Instance.Modifiers, MemLimitMB: manifest.Instance.MemLimitMB,
		CPULimit: manifest.Instance.CPULimit, ExtraArgs: manifest.Instance.ExtraArgs,
		StartAfterProvision: body.StartAfterProvision,
		Mods:                make([]resolveRequest, 0, len(manifest.Mods)),
	}
	mods, err := h.packsFirst(r.Context(), manifest.Mods)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	sides := map[string]string{}
	for _, mod := range mods {
		create.Mods = append(create.Mods, resolveRequest{
			FullName: mod.FullName, Version: mod.Version, Source: mod.Source,
		})
		if mod.Side != "" && mod.Side != store.SideUnknown {
			sides[mod.FullName] = mod.Side
		}
	}
	// The pinned versions are checked by the create path's own resolver pass, which refuses a
	// package the index cannot supply before the row or the port is claimed (Q42).
	h.createInstance(
		w,
		r,
		u,
		create,
		control.OperationImport,
		&control.OperationPlan{Configs: manifest.Configs, Sides: sides},
	)
}

// packsFirst moves each modpack ahead of the other mods, so the mods it bundles install as its
// dependencies and keep following it, rather than each becoming an install of its own.
func (h *Instances) packsFirst(ctx context.Context, mods []manifestMod) ([]manifestMod, error) {
	packs := make([]manifestMod, 0, len(mods))
	rest := make([]manifestMod, 0, len(mods))
	for _, mod := range mods {
		rows, err := h.DB.ModPackagesByFullName(ctx, mod.FullName)
		if err != nil {
			return nil, fmt.Errorf("look up %s in the catalogue: %w", mod.FullName, err)
		}
		pack := false
		for i := range rows {
			pack = pack || manager.IsPack(&rows[i])
		}
		if pack {
			packs = append(packs, mod)
		} else {
			rest = append(rest, mod)
		}
	}
	return append(packs, rest...), nil
}

// validateManifest reports every reason the document cannot be applied. It is the one place
// the bounds, the side vocabulary and the filename rule live, so the preview and the import
// cannot disagree about what would be refused.
func validateManifest(m *instanceManifest) []manifestProblem {
	problems := []manifestProblem{}
	if m.Schema != manifestSchema {
		problems = append(problems, manifestProblem{
			Field:  "schema",
			Detail: fmt.Sprintf("This panel reads manifest schema %d, not %d.", manifestSchema, m.Schema),
		})
		// Every rule below describes schema 1's shape, so there is nothing more to say about
		// a document that is not one.
		return problems
	}
	if len(m.Mods) > maxManifestMods {
		problems = append(problems, manifestProblem{
			Field:  "mods",
			Detail: fmt.Sprintf("%d mods; this panel imports at most %d.", len(m.Mods), maxManifestMods),
		})
	}
	for _, mod := range m.Mods {
		if strings.TrimSpace(mod.FullName) == "" || strings.TrimSpace(mod.Version) == "" {
			problems = append(problems, manifestProblem{
				Field: "mods", Detail: "Every mod needs a full_name and a version.",
			})
			break
		}
	}
	for i, mod := range m.Mods {
		problems = append(problems, modProblems(i, mod)...)
	}
	if len(m.Configs) > maxManifestConfigs {
		problems = append(problems, manifestProblem{
			Field: "configs",
			Detail: fmt.Sprintf("%d config files; this panel imports at most %d.",
				len(m.Configs), maxManifestConfigs),
		})
	}
	seen := map[string]bool{}
	for _, cfg := range m.Configs {
		if err := control.CheckManifestConfigName(cfg.File); err != nil {
			problems = append(problems, manifestProblem{Field: "configs", Detail: err.Error()})
			continue
		}
		if seen[cfg.File] {
			problems = append(problems, manifestProblem{
				Field: "configs", Detail: cfg.File + " appears twice.",
			})
		}
		seen[cfg.File] = true
		if len(cfg.Content) > control.MaxConfigSize {
			problems = append(problems, manifestProblem{
				Field:  "configs",
				Detail: fmt.Sprintf("%s is larger than %d bytes.", cfg.File, control.MaxConfigSize),
			})
		}
	}
	return problems
}

// modProblems is what is wrong with one manifest mod's registry and side tag.
func modProblems(i int, mod manifestMod) []manifestProblem {
	var problems []manifestProblem
	if _, ok := source.ByName(mod.Source); mod.Source != "" && !ok {
		problems = append(problems, manifestProblem{
			Field:  fmt.Sprintf("mods[%d].source", i),
			Detail: fmt.Sprintf("%s names the registry %q, which this panel does not know.", mod.FullName, mod.Source),
		})
	}
	if mod.Side != "" && !sides[mod.Side] {
		problems = append(problems, manifestProblem{
			Field: fmt.Sprintf("mods[%d].side", i), Detail: fmt.Sprintf("%s has side %q; a side is "+
				"one of server_only, client_required, client_optional, unknown.", mod.FullName, mod.Side),
		})
	}
	return problems
}
