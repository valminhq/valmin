package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/profile"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
)

const (
	// maxProfileBytes bounds the text a host stores for one profile code.
	maxProfileBytes = 16 << 20
	// maxProfileManifestBytes bounds export.r2x inside a downloaded profile.
	maxProfileManifestBytes = 4 << 20
	// frameworkConfigFile is the loader's own config. A client's copy would override the logging
	// the panel turns on to read plugin lines, so a profile never brings it.
	frameworkConfigFile = "BepInEx.cfg"
	// profileWorldName is the world a server imported from a profile starts with.
	profileWorldName = "Dedicated"
)

// profileHosts holds the host that stores profile codes for each enabled registry.
type profileHosts map[source.Source]*profile.Host

// profileManifest turns the r2modman or Gale profile stored under key into a definition.
// Its enabled mods follow the dependency order of profileMods and its config files come along
// whole; the caller merges them like a template code's settings.
func (h *Instances) profileManifest(ctx context.Context, key string) (*instanceManifest, int, error) {
	archive, origin, err := fetchProfile(ctx, h.Profiles, key)
	if err != nil {
		return nil, 0, err
	}
	p, err := profile.Read(archive, profile.Limits{
		ManifestBytes: maxProfileManifestBytes, Configs: maxManifestConfigs,
		ConfigBytes: control.MaxConfigSize, TotalBytes: maxManifestBytes,
	})
	if err != nil {
		return nil, 0, codeInvalid("This profile could not be read: " + err.Error() + ".")
	}

	name := cmp.Or(p.Name, "Imported profile")
	m := &instanceManifest{
		Schema:   manifestSchema,
		Name:     name,
		Instance: control.ManifestLaunch{ServerName: name, WorldName: profileWorldName},
	}
	if name == profileWorldName {
		m.Instance.WorldName = profileWorldName + " world"
	}
	for _, mod := range p.Mods {
		if !mod.Enabled {
			continue
		}
		registry, err := h.profileRegistry(ctx, origin, mod)
		if err != nil {
			return nil, 0, apierr.New(errcode.Internal).Wrap(err)
		}
		m.Mods = append(m.Mods, control.ManifestMod{FullName: mod.FullName, Version: mod.Version, Source: registry})
	}
	for _, c := range p.Configs {
		if c.File == frameworkConfigFile {
			continue
		}
		m.Configs = append(m.Configs, control.ManifestConfig{File: c.File, Content: string(c.Content)})
	}
	m.Configs = portableConfigs(m.Configs)
	if len(m.Mods) > maxManifestMods {
		return m, len(m.Mods), nil
	}
	mods, listed, err := profileMods(m.Mods, h.catalogueDependencies(ctx))
	if err != nil {
		return nil, 0, apierr.New(errcode.Internal).Wrap(err)
	}
	m.Mods = mods
	return m, listed, nil
}

// profileRegistry is the registry a profile mod installs from. A registry the profile names is kept
// as written, so one this panel does not know is refused with the rest of the definition. A mod
// that names none comes from the registry that served the code when that registry carries the
// exact version, and otherwise from whichever enabled registry does.
func (h *Instances) profileRegistry(ctx context.Context, origin source.Source, mod profile.Mod) (string, error) {
	if mod.Source != "" {
		return mod.Source, nil
	}
	_, _, carried, err := h.DB.ModVersionDependenciesFrom(
		ctx, mod.FullName, mod.Version, source.Source{}, []source.Source{origin})
	if err != nil {
		return "", fmt.Errorf("look up %s %s: %w", mod.FullName, mod.Version, err)
	}
	if carried {
		return origin.String(), nil
	}
	return "", nil
}

// fetchProfile downloads the archive stored under key from the first enabled registry that has
// it, and returns that registry. A profile no registry has is a rejected code; a registry that
// failed to answer, with no other having the code, makes the lookup unavailable.
func fetchProfile(ctx context.Context, hosts profileHosts, key string) ([]byte, source.Source, error) {
	var failure error
	for _, src := range source.All() {
		host, ok := hosts[src]
		if !ok {
			continue
		}
		archive, err := host.Fetch(ctx, key, maxProfileBytes)
		switch {
		case err == nil:
			return archive, src, nil
		case errors.Is(err, profile.ErrNotFound):
		case errors.Is(err, profile.ErrTooLarge):
			return nil, source.Source{}, codeInvalid("This profile is larger than this panel accepts.")
		default:
			failure = fmt.Errorf("fetch profile from %s: %w", src, err)
		}
	}
	if failure != nil {
		return nil, source.Source{}, apierr.New(errcode.Unavailable).
			Msg("The profile could not be downloaded from the mod registry. Try again in a moment.").
			Wrap(failure)
	}
	return nil, source.Source{}, codeInvalid("No r2modman or Gale profile has this code; it may have expired.")
}

func codeInvalid(detail string) error {
	var val apierr.Validation
	val.Add("code", apierr.FieldInvalid, detail)
	return val.Err() //nolint:wrapcheck // the validation error is the response
}

// profileMods puts first the mods no other mod in the profile depends on, which install as the
// explicit choices, and then the rest in profile order, which install as the dependencies they
// are. listed counts the first group. A mod the catalogue cannot describe counts as a choice.
func profileMods(mods []control.ManifestMod, lookup dependencyLookup) ([]control.ManifestMod, int, error) {
	needed := map[string]bool{}
	for _, m := range mods {
		deps, _, ok, err := lookup(m.FullName, m.Version, m.Source)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			continue
		}
		for _, ident := range deps {
			if name, _, ok := modresolver.ParseDependency(ident); ok && name != m.FullName {
				needed[name] = true
			}
		}
	}
	chosen := make([]control.ManifestMod, 0, len(mods))
	rest := make([]control.ManifestMod, 0, len(mods))
	for _, m := range mods {
		if needed[m.FullName] {
			rest = append(rest, m)
		} else {
			chosen = append(chosen, m)
		}
	}
	return append(chosen, rest...), len(chosen), nil
}
