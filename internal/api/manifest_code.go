package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/manager"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/sharecode"
	"github.com/valminhq/valmin/internal/store"
)

// templateCodeView is GET /instances/{id}/manifest/code.
type templateCodeView struct {
	Code            string `json:"code"`
	Mods            int    `json:"mods"`
	Settings        int    `json:"settings"`
	SecretsLeftOut  int    `json:"secrets_left_out"`
	DisabledLeftOut int    `json:"disabled_left_out"`
}

// exportManifestCode is GET /instances/{id}/manifest/code: the server as a template code with
// its enabled mods and the config settings changed in the panel.
func (h *Instances) exportManifestCode(w http.ResponseWriter, r *http.Request) {
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

	view, err := h.templateCode(r.Context(), inst)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, view)
}

func (h *Instances) templateCode(ctx context.Context, inst *store.Instance) (*templateCodeView, error) {
	installed, err := h.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return nil, fmt.Errorf("read installed mods for instance %s: %w", inst.ID, err)
	}
	view := &templateCodeView{}
	enabled := slices.DeleteFunc(installed, func(m store.InstanceMod) bool {
		if !m.Enabled {
			view.DisabledLeftOut++
		}
		return !m.Enabled
	})
	view.Mods = len(enabled)
	mods, err := reduceMods(enabled, h.catalogueDependencies(ctx))
	if err != nil {
		return nil, err
	}
	configs, err := readInstanceTweaks(inst, view)
	if err != nil {
		return nil, err
	}
	view.Code, err = sharecode.Encode(&sharecode.Template{
		Name: inst.Name, Launch: control.LaunchOf(inst), Mods: mods, Configs: configs,
	})
	if err != nil {
		return nil, fmt.Errorf("encode template code: %w", err)
	}
	return view, nil
}

// readInstanceTweaks is the settings of each portable config that differ from the copy kept
// before the panel's first write. A file the panel never wrote has no such copy and is skipped.
func readInstanceTweaks(inst *store.Instance, view *templateCodeView) ([]control.ManifestConfig, error) {
	full, err := readInstanceConfigs(inst)
	if err != nil || len(full) == 0 {
		return nil, err
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err != nil {
		return nil, fmt.Errorf("read config directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	var out []control.ManifestConfig
	for _, cfg := range full {
		orig, _, err := fsutil.ReadRegularIn(dir, cfg.File+modconfig.OriginalSuffix)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the original of %s: %w", cfg.File, err)
		}
		tweaks, kept, secrets := modconfig.Tweaks(orig, []byte(cfg.Content))
		view.Settings += kept
		view.SecretsLeftOut += secrets
		if kept > 0 {
			out = append(out, control.ManifestConfig{File: cfg.File, Content: string(tweaks)})
		}
	}
	return out, nil
}

// requestManifest is the definition an import request carries, from its manifest or its code.
// A code's mods are followed by the dependencies it leaves out; listed counts the mods before them.
func (h *Instances) requestManifest(ctx context.Context, body *importRequest) (*instanceManifest, int, error) {
	var val apierr.Validation
	switch {
	case body.Manifest != nil && body.Code != "":
		val.Add("code", apierr.FieldInvalid, "Send a manifest or a code, not both.")
		return nil, 0, val.Err() //nolint:wrapcheck // the validation error is the response
	case body.Manifest != nil:
		body.Manifest.Configs = portableConfigs(body.Manifest.Configs)
		return body.Manifest, len(body.Manifest.Mods), nil
	case body.Code == "":
		val.Add("manifest", apierr.FieldRequired, "A manifest or a code is required.")
		return nil, 0, val.Err() //nolint:wrapcheck // the validation error is the response
	}
	t, err := sharecode.Decode(body.Code, maxManifestBytes)
	if err != nil {
		val.Add("code", apierr.FieldInvalid, capitalize(err.Error())+".")
		return nil, 0, val.Err() //nolint:wrapcheck // the validation error is the response
	}
	m := &instanceManifest{
		Schema: manifestSchema, Name: t.Name, Instance: t.Launch,
		Mods: t.Mods, Configs: portableConfigs(t.Configs),
	}
	listed := len(m.Mods)
	if listed > maxManifestMods {
		return m, listed, nil
	}
	if m.Mods, err = expandMods(m.Mods, h.catalogueDependencies(ctx)); err != nil {
		return nil, 0, apierr.New(errcode.Internal).Wrap(err)
	}
	return m, listed, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// dependencyLookup returns the dependency pins of one package version, the registry it was
// found in, and whether the catalogue has it.
type dependencyLookup func(fullName, version, src string) (deps []string, foundIn string, ok bool, err error)

// catalogueDependencies looks versions up in this panel's catalogue, in the named registry when
// there is one. Results are cached for the life of the returned function.
func (h *Instances) catalogueDependencies(ctx context.Context) dependencyLookup {
	type entry struct {
		deps    []string
		foundIn string
		ok      bool
	}
	cache := map[string]entry{}
	return func(fullName, version, src string) ([]string, string, bool, error) {
		key := fullName + "\x00" + version + "\x00" + src
		if e, hit := cache[key]; hit {
			return e.deps, e.foundIn, e.ok, nil
		}
		allowed := source.All()
		if s, ok := source.ByName(src); ok {
			allowed = []source.Source{s}
		}
		deps, foundIn, ok, err := h.DB.ModVersionDependenciesFrom(ctx, fullName, version, source.Source{}, allowed)
		if err != nil {
			return nil, "", false, fmt.Errorf("look up %s %s: %w", fullName, version, err)
		}
		cache[key] = entry{deps, foundIn.String(), ok}
		return deps, foundIn.String(), ok, nil
	}
}

// expandMods adds every package the mods' dependency pins reach that they do not list: at the
// highest version pinned, with the strongest side tag of the packages that reach it, from the
// registry of the first. Listed mods keep their own values. Added packages follow, by name.
func expandMods(mods []control.ManifestMod, lookup dependencyLookup) ([]control.ManifestMod, error) {
	listed := make(map[string]bool, len(mods))
	for _, m := range mods {
		listed[m.FullName] = true
	}
	derived := map[string]*control.ManifestMod{}
	for changed := true; changed; {
		changed = false
		for _, m := range append(slices.Clone(mods), sortedMods(derived)...) {
			deps, foundIn, ok, err := lookup(m.FullName, m.Version, m.Source)
			if err != nil {
				return nil, err
			}
			if ok && deriveFrom(derived, listed, m, deps, foundIn) {
				changed = true
			}
		}
	}
	return append(slices.Clone(mods), sortedMods(derived)...), nil
}

// deriveFrom raises the derived packages that m's dependency pins reach and reports whether any
// changed.
func deriveFrom(
	derived map[string]*control.ManifestMod, listed map[string]bool,
	m control.ManifestMod, deps []string, foundIn string,
) bool {
	changed := false
	for _, ident := range deps {
		name, version, ok := modresolver.ParseDependency(ident)
		if !ok || listed[name] {
			continue
		}
		d := derived[name]
		if d == nil {
			derived[name] = &control.ManifestMod{FullName: name, Version: version, Source: foundIn, Side: m.Side}
			changed = true
			continue
		}
		if higherVersion(version, d.Version) {
			d.Version, changed = version, true
		}
		if manager.Weaker(d.Side, m.Side) {
			d.Side, changed = m.Side, true
		}
	}
	return changed
}

// reduceMods lists rows as template mods, leaving out each dependency that expandMods derives
// exactly from the rest.
func reduceMods(rows []store.InstanceMod, lookup dependencyLookup) ([]control.ManifestMod, error) {
	want := make([]control.ManifestMod, 0, len(rows))
	var listed []control.ManifestMod
	for i := range rows {
		m := control.ManifestMod{
			FullName: rows[i].FullName, Source: rows[i].Source.String(),
			Version: rows[i].Version, Side: rows[i].Side,
		}
		want = append(want, m)
		if rows[i].InstalledAs != store.InstalledDependency {
			listed = append(listed, m)
		}
	}
	for {
		full, err := expandMods(listed, lookup)
		if err != nil {
			return nil, err
		}
		got := make(map[string]control.ManifestMod, len(full))
		for _, m := range full {
			got[m.FullName] = m
		}
		var missing []control.ManifestMod
		for _, m := range want {
			if g, ok := got[m.FullName]; !ok || !sameMod(g, m) {
				missing = append(missing, m)
			}
		}
		if len(missing) == 0 {
			return listed, nil
		}
		listed = append(listed, missing...)
	}
}

func sameMod(a, b control.ManifestMod) bool {
	return a.Version == b.Version && a.Source == b.Source && sideOrUnknown(a.Side) == sideOrUnknown(b.Side)
}

func sideOrUnknown(side string) string {
	if side == "" {
		return store.SideUnknown
	}
	return side
}

func higherVersion(a, b string) bool {
	va, okA := semver.ParseVersion(a)
	vb, okB := semver.ParseVersion(b)
	return okA && okB && semver.Compare(va, vb) > 0
}

func sortedMods(mods map[string]*control.ManifestMod) []control.ManifestMod {
	out := make([]control.ManifestMod, 0, len(mods))
	for _, m := range mods {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b control.ManifestMod) int { return strings.Compare(a.FullName, b.FullName) })
	return out
}
