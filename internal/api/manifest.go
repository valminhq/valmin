package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/mods/fsutil"
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
	maxManifestBytes      = 8 << 20
	maxManifestMods       = 200
	maxManifestConfigs    = 200
	maxManifestConfigSize = 1 << 20
)

// manifestLaunch is the part of an instances row that defines the server rather than
// identifying this installation. The omissions are the point: no id, no port, no
// crossplay_instance_id, no container, no build, no password (ADR-151).
type manifestLaunch struct {
	ServerName      string            `json:"server_name"`
	WorldName       string            `json:"world_name"`
	Public          bool              `json:"public"`
	Crossplay       bool              `json:"crossplay"`
	Preset          string            `json:"preset,omitempty"`
	Modifiers       map[string]string `json:"modifiers,omitempty"`
	ExtraArgs       string            `json:"extra_args,omitempty"`
	MemLimitMB      int               `json:"mem_limit_mb"`
	CPULimit        *float64          `json:"cpu_limit"`
	BackupKeepCold  int               `json:"backup_keep_cold"`
	BackupKeepHot   int               `json:"backup_keep_hot"`
	BackupOnRestart bool              `json:"backup_on_restart"`
}

// manifestMod is one pinned package. The side tag travels because it is the admin's own
// classification (03 §5.6) and re-tagging a restored server by hand is work nobody recorded.
type manifestMod struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	Side     string `json:"side,omitempty"`
}

// manifestConfig is one .cfg file, whole. File is a bare filename, validated against the
// instance's config directory on the way in.
type manifestConfig struct {
	File    string `json:"file"`
	Content string `json:"content"`
}

type instanceManifest struct {
	Schema   int              `json:"schema"`
	Name     string           `json:"name"`
	Instance manifestLaunch   `json:"instance"`
	Mods     []manifestMod    `json:"mods"`
	Configs  []manifestConfig `json:"configs"`
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
	Name     string            `json:"name"`
	Instance manifestLaunch    `json:"instance"`
	Mods     []previewModView  `json:"mods"`
	Configs  []previewFileView `json:"configs"`
	Problems []manifestProblem `json:"problems"`
}

type previewModView struct {
	FullName string `json:"full_name"`
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	for _, action := range []authz.Action{authz.InstanceSettings, authz.ModsList, authz.ConfigRead} {
		if !h.Authz.Can(r.Context(), u, action, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}

	installed, err := h.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	configs, err := readInstanceConfigs(inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	manifest := &instanceManifest{
		Schema:   manifestSchema,
		Name:     inst.Name,
		Instance: launchOf(inst),
		Mods:     make([]manifestMod, 0, len(installed)),
		Configs:  configs,
	}
	for i := range installed {
		manifest.Mods = append(manifest.Mods, manifestMod{
			FullName: installed[i].FullName, Version: installed[i].Version, Side: installed[i].Side,
		})
	}
	JSON(w, r, http.StatusOK, manifest)
}

// launchOf reads the launch half of an instances row. Modifiers are stored as JSON text; a row
// that will not decode exports without them rather than failing the whole manifest, since the
// column is the panel's own and an unreadable one is a bug to see, not a reason to withhold
// every other field.
func launchOf(inst *store.Instance) manifestLaunch {
	launch := manifestLaunch{
		ServerName: inst.ServerName, WorldName: inst.WorldName,
		Public: inst.Public, Crossplay: inst.Crossplay,
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
		BackupKeepCold: inst.BackupKeepCold, BackupKeepHot: inst.BackupKeepHot,
		BackupOnRestart: inst.BackupOnRestart,
	}
	if inst.Preset != nil {
		launch.Preset = *inst.Preset
	}
	if inst.ExtraArgs != nil {
		launch.ExtraArgs = *inst.ExtraArgs
	}
	if inst.Modifiers != nil && *inst.Modifiers != "" {
		_ = json.Unmarshal([]byte(*inst.Modifiers), &launch.Modifiers)
	}
	return launch
}

// readInstanceConfigs reads every .cfg in the instance's config directory whole. A server that
// has never started has none, which is an empty list rather than an error (03 §9).
func readInstanceConfigs(inst *store.Instance) ([]manifestConfig, error) {
	dir := filepath.Join(serverDir(inst), filepath.FromSlash(configDir))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []manifestConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config directory: %w", err)
	}
	out := []manifestConfig{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cfg") {
			continue
		}
		//nolint:gosec // dir is the instance's own config directory and e.Name() came from it
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", e.Name(), err)
		}
		out = append(out, manifestConfig{File: e.Name(), Content: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
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
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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

	preview := manifestPreview{
		Name:     manifest.Name,
		Instance: manifest.Instance,
		Mods:     []previewModView{},
		Configs:  []previewFileView{},
		Problems: validateManifest(manifest),
	}
	for _, mod := range manifest.Mods {
		_, available, err := h.DB.ModVersionDependencies(r.Context(), mod.FullName, mod.Version)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		preview.Mods = append(preview.Mods, previewModView{
			FullName: mod.FullName, Version: mod.Version, Side: mod.Side, Available: available,
		})
		if !available {
			preview.Problems = append(preview.Problems, manifestProblem{
				Field:  "mods",
				Detail: mod.FullName + " " + mod.Version + " is not in the catalogue.",
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
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
	for _, mod := range manifest.Mods {
		create.Mods = append(create.Mods, resolveRequest{FullName: mod.FullName, Version: mod.Version})
	}
	// The pinned versions are checked by the create path's own resolver pass, which refuses a
	// package the index cannot supply before the row or the port is claimed (Q42).
	h.createInstance(w, r, u, create, manifest.Configs)
}

// validateManifest reports every reason the document cannot be applied. It is the one place
// the bounds and the filename rule live, so the preview and the import cannot disagree about
// what would be refused.
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
	if len(m.Configs) > maxManifestConfigs {
		problems = append(problems, manifestProblem{
			Field: "configs",
			Detail: fmt.Sprintf("%d config files; this panel imports at most %d.",
				len(m.Configs), maxManifestConfigs),
		})
	}
	seen := map[string]bool{}
	for _, cfg := range m.Configs {
		if err := checkManifestConfigName(cfg.File); err != nil {
			problems = append(problems, manifestProblem{Field: "configs", Detail: err.Error()})
			continue
		}
		if seen[cfg.File] {
			problems = append(problems, manifestProblem{
				Field: "configs", Detail: cfg.File + " appears twice.",
			})
		}
		seen[cfg.File] = true
		if len(cfg.Content) > maxManifestConfigSize {
			problems = append(problems, manifestProblem{
				Field:  "configs",
				Detail: fmt.Sprintf("%s is larger than %d bytes.", cfg.File, maxManifestConfigSize),
			})
		}
	}
	return problems
}

// checkManifestConfigName is 03 §6.5's archive-entry rule applied to a manifest: the name must
// be one plain .cfg file, so it cannot escape the config directory or name something the
// config editor would never have written.
func checkManifestConfigName(name string) error {
	if name == "" {
		return fmt.Errorf("a config entry has no filename")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not a plain filename", name)
	}
	if !strings.HasSuffix(name, ".cfg") {
		return fmt.Errorf("%q is not a .cfg file", name)
	}
	return nil
}

// applyManifestConfigs writes an import's config bytes into the freshly provisioned instance.
// Names are re-checked here rather than trusted from the payload: this runs in a job, long
// after the request that validated them, and the check is three comparisons.
func applyManifestConfigs(inst *store.Instance, configs []manifestConfig) error {
	if len(configs) == 0 {
		return nil
	}
	dir := filepath.Join(serverDir(inst), filepath.FromSlash(configDir))
	if err := fsutil.MkdirAllExact(dir); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	for _, cfg := range configs {
		if err := checkManifestConfigName(cfg.File); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, cfg.File), []byte(cfg.Content)); err != nil {
			return fmt.Errorf("write config %s: %w", cfg.File, err)
		}
	}
	return nil
}
