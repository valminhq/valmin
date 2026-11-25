package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"
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

// The manifest versions this build reads and writes; any other value is refused rather than
// best-effort parsed. Schema 1 carries only plain .cfg names directly in BepInEx/config; schema 2
// also carries paths into its subfolders and YAML, JSON and text files. Export writes 1 when
// the file needs nothing more, so older panels can still read it.
const (
	manifestSchema      = 1
	manifestSchemaPaths = 2
)

// configsField names the manifest's config files in a problem.
const configsField = "configs"

// The manifest's bounds, enforced on the way in. An importer reads untrusted bytes, so every
// dimension it could grow along has a ceiling (ADR-151).
const (
	// maxManifestBytes is the whole document's cap, applied by the handler because 11 §8.3's
	// 1 MiB JSON limit is exempted for these two routes — a real modpack's config is bigger
	// than that, and a manifest missing config is not a definition (ADR-151).
	maxManifestBytes = 8 << 20
	// importWrapperBytes is room kept, under maxManifestBytes, for an import request's fields
	// around the document.
	importWrapperBytes = 64 << 10
	maxManifestMods    = 1000
	maxManifestConfigs = 200
)

type instanceManifest struct {
	Schema   int                      `json:"schema"`
	Name     string                   `json:"name"`
	Instance control.ManifestLaunch   `json:"instance"`
	Mods     []control.ManifestMod    `json:"mods"`
	Configs  []control.ManifestConfig `json:"configs"`
}

// importRequest is POST /instances/import. Name and password come from the caller, never from
// the file: the manifest carries no password, and a name has to be free on this panel.
type importRequest struct {
	Manifest            *instanceManifest `json:"manifest"`
	Code                string            `json:"code,omitempty"`
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

// previewFileView is one config file an import writes. Settings lists a code's settings for the
// file, marking the credential-looking ones; a definition file's configs are whole and carry none.
type previewFileView struct {
	File     string                `json:"file"`
	Bytes    int                   `json:"size_bytes"`
	Settings []templateSettingView `json:"settings,omitempty"`
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

	manifest, err := instanceDefinition(r.Context(), h.DB, inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	detail, err := importableDetail(manifest)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if detail != "" {
		apierr.Write(w, r, apierr.New(errcode.ValidationFailed).Msg(detail))
		return
	}
	JSON(w, r, http.StatusOK, manifest)
}

// importableDetail says why no panel would import m, which an export refuses rather than hands
// out: more config files than an import accepts, or a document larger than an import request may
// be. It is empty when m fits.
func importableDetail(m *instanceManifest) (string, error) {
	if len(m.Configs) > maxManifestConfigs {
		return fmt.Sprintf("This server has %d changed config files, more than a template file can "+
			"carry (%d). Restore their originals in the config editor, or share a template code.",
			len(m.Configs), maxManifestConfigs), nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode definition: %w", err)
	}
	// The import request wraps the document with a name and a password.
	if len(raw) > maxManifestBytes-importWrapperBytes {
		return fmt.Sprintf("This server's changed config files come to %d MiB, more than a template "+
			"file can carry. Restore the largest to their originals, or share a template code.",
			len(raw)>>20), nil
	}
	return "", nil
}

// instanceDefinition is the server's definition for export: launch settings, installed mods,
// and the config files the panel changed, whole, apart from a .cfg too large to carry.
func instanceDefinition(ctx context.Context, db *store.DB, inst *store.Instance) (*instanceManifest, error) {
	installed, err := db.InstanceMods(ctx, inst.ID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods for instance %s: %w", inst.ID, err)
	}
	carried, _, err := templateFiles(ctx, db, inst)
	if err != nil {
		return nil, err
	}
	configs := make([]control.ManifestConfig, 0, len(carried))
	for _, f := range carried {
		if !f.TooLarge {
			configs = append(configs, control.ManifestConfig{File: f.File, Content: string(f.Content)})
		}
	}
	launch, err := control.LaunchOf(inst)
	if err != nil {
		return nil, fmt.Errorf("read launch settings: %w", err)
	}
	manifest := &instanceManifest{
		Schema:   schemaFor(configs),
		Name:     inst.Name,
		Instance: launch,
		Mods:     make([]control.ManifestMod, 0, len(installed)),
		Configs:  configs,
	}
	for i := range installed {
		manifest.Mods = append(manifest.Mods, control.ManifestMod{
			FullName: installed[i].FullName, Source: installed[i].Source.String(),
			Version: installed[i].Version, Side: installed[i].Side,
		})
	}
	return manifest, nil
}

// nestedConfigProblems names each config file whose path runs through another config file of
// the same definition, which no import could write. names are relative paths the name check
// accepted, so walking up each one ends at ".".
func nestedConfigProblems(names []string) []manifestProblem {
	files := make(map[string]bool, len(names))
	for _, name := range names {
		files[name] = true
	}
	var problems []manifestProblem
	for _, name := range names {
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if files[dir] {
				problems = append(problems, manifestProblem{
					Field: configsField, Detail: fmt.Sprintf("%s is inside %s, which is a file.", name, dir),
				})
				break
			}
		}
	}
	return problems
}

// schemaFor is the manifest schema that carries configs: 1 when every file is a plain .cfg name,
// 2 otherwise.
func schemaFor(configs []control.ManifestConfig) int {
	for _, c := range configs {
		if control.CheckManifestConfigName(c.File) != nil {
			return manifestSchemaPaths
		}
	}
	return manifestSchema
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
	manifest, _, err := h.requestManifest(r.Context(), &body)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

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
		file := previewFileView{File: cfg.File, Bytes: len(cfg.Content)}
		if body.Code != "" {
			file.Settings = tweakView([]byte(cfg.Content)).Settings
		}
		preview.Configs = append(preview.Configs, file)
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

	manifest, listed, err := h.requestManifest(r.Context(), &body)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	var val apierr.Validation
	for _, problem := range validateManifest(manifest) {
		val.Add(problem.Field, apierr.FieldInvalid, problem.Detail)
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return
	}

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
	// Derived dependencies go last, so each is already in place as a dependency when its own
	// request runs.
	mods, err := h.packsFirst(r.Context(), manifest.Mods[:listed])
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	mods = append(mods, manifest.Mods[listed:]...)
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
		&control.OperationPlan{Configs: manifest.Configs, MergeConfigs: body.Code != "", Sides: sides},
	)
}

// packsFirst moves each modpack ahead of the other mods, so the mods it bundles install as its
// dependencies and keep following it, rather than each becoming an install of its own.
func (h *Instances) packsFirst(ctx context.Context, mods []control.ManifestMod) ([]control.ManifestMod, error) {
	packs := make([]control.ManifestMod, 0, len(mods))
	rest := make([]control.ManifestMod, 0, len(mods))
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
	if m.Schema != manifestSchema && m.Schema != manifestSchemaPaths {
		problems = append(problems, manifestProblem{
			Field: "schema",
			Detail: fmt.Sprintf("This panel reads manifest schema %d or %d, not %d.",
				manifestSchema, manifestSchemaPaths, m.Schema),
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
			Field: configsField,
			Detail: fmt.Sprintf("%d config files; this panel imports at most %d.",
				len(m.Configs), maxManifestConfigs),
		})
	}
	seen := map[string]bool{}
	checkName := control.CheckManifestConfigName
	if m.Schema == manifestSchemaPaths {
		checkName = instance.CheckConfigPath
	}
	valid := make([]string, 0, len(m.Configs))
	for _, cfg := range m.Configs {
		if err := checkName(cfg.File); err != nil {
			problems = append(problems, manifestProblem{Field: configsField, Detail: err.Error()})
			continue
		}
		valid = append(valid, cfg.File)
		if seen[cfg.File] {
			problems = append(problems, manifestProblem{
				Field: configsField, Detail: cfg.File + " appears twice.",
			})
		}
		seen[cfg.File] = true
		if len(cfg.Content) > instance.MaxConfigSize {
			problems = append(problems, manifestProblem{
				Field:  configsField,
				Detail: fmt.Sprintf("%s is larger than %d bytes.", cfg.File, instance.MaxConfigSize),
			})
		}
	}
	return append(problems, nestedConfigProblems(valid)...)
}

// modProblems is what is wrong with one manifest mod's registry and side tag.
func modProblems(i int, mod control.ManifestMod) []manifestProblem {
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
