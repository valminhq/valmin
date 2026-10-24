// Package profile reads and writes the mod profile archive that r2modman, Gale and Thunderstore
// Mod Manager exchange: a zip holding export.r2x, a YAML list of package versions, and the
// profile's BepInEx config files.
package profile

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/valminhq/valmin/internal/mods/semver"
)

const (
	manifestFile = "export.r2x"
	thunderstore = "thunderstore"
)

// Mod is one package version in a profile. Source names the registry it came from; empty when
// the profile names none, as r2modman profiles do.
type Mod struct {
	FullName string
	Version  string
	Source   string
	Enabled  bool
}

// Config is one config file from a profile. File is its bare name inside BepInEx/config.
type Config struct {
	File    string
	Content []byte
}

// Profile is what an archive holds.
type Profile struct {
	Name    string
	Mods    []Mod
	Configs []Config
}

// Limits bound what Read accepts from an archive it did not make.
type Limits struct {
	ManifestBytes int64
	Configs       int
	ConfigBytes   int64
	// TotalBytes bounds all the config files together.
	TotalBytes int64
}

// ArchiveTime stamps every entry Archive writes, so two archives of the same mods are identical.
var ArchiveTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// manifest is the YAML document. Gale writes a version as versionNumber in some builds.
type manifest struct {
	ProfileName string        `yaml:"profileName"`
	Mods        []manifestMod `yaml:"mods"`
}

type manifestMod struct {
	Name          string   `yaml:"name"`
	Version       *version `yaml:"version"`
	VersionNumber *version `yaml:"versionNumber"`
	Enabled       *bool    `yaml:"enabled"`
	Source        string   `yaml:"source"`
}

type version struct {
	Major int `yaml:"major"`
	Minor int `yaml:"minor"`
	Patch int `yaml:"patch"`
}

// Read decodes a profile archive. Only export.r2x and the config files directly inside
// BepInEx/config (or the older config/) are read; every other entry is ignored unread.
func Read(archive []byte, lim Limits) (*Profile, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open profile archive: %w", err)
	}
	var (
		doc     *manifest
		configs []Config
		total   int64
		seen    = map[string]bool{}
	)
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if name == manifestFile {
			if doc, err = readManifest(f, lim.ManifestBytes); err != nil {
				return nil, err
			}
			continue
		}
		file, ok := configName(name)
		if !ok || seen[file] {
			continue
		}
		seen[file] = true
		if len(configs) == lim.Configs {
			return nil, fmt.Errorf("profile holds more than %d config files", lim.Configs)
		}
		content, err := readLimited(f, lim.ConfigBytes)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if total += int64(len(content)); total > lim.TotalBytes {
			return nil, fmt.Errorf("config files are larger than %d bytes together", lim.TotalBytes)
		}
		configs = append(configs, Config{File: file, Content: content})
	}
	if doc == nil {
		return nil, errors.New("profile archive has no " + manifestFile)
	}
	p := &Profile{Name: doc.ProfileName, Configs: configs}
	for _, m := range doc.Mods {
		mod, err := m.mod()
		if err != nil {
			return nil, err
		}
		p.Mods = append(p.Mods, mod)
	}
	return p, nil
}

// configName is the bare file name of a .cfg directly inside BepInEx/config or config.
func configName(path string) (string, bool) {
	for _, dir := range []string{"BepInEx/config/", "config/"} {
		if name, ok := strings.CutPrefix(path, dir); ok {
			return name, name != ".cfg" && !strings.Contains(name, "/") && strings.HasSuffix(name, ".cfg")
		}
	}
	return "", false
}

func readManifest(f *zip.File, limit int64) (*manifest, error) {
	raw, err := readLimited(f, limit)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestFile, err)
	}
	var doc manifest
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", manifestFile, err)
	}
	return &doc, nil
}

// readLimited reads one archive entry, refusing one larger than limit.
func readLimited(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open entry: %w", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read entry: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return raw, nil
}

func (m manifestMod) mod() (Mod, error) {
	v := m.Version
	if v == nil {
		v = m.VersionNumber
	}
	if m.Name == "" || v == nil {
		return Mod{}, errors.New("profile lists a mod without a name or version")
	}
	return Mod{
		FullName: m.Name,
		Version:  fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch),
		Source:   m.Source,
		Enabled:  m.Enabled == nil || *m.Enabled,
	}, nil
}

// Archive writes the mods as a profile archive holding export.r2x only. The YAML is written
// directly: every value is an integer or a package name, and only the profile name needs
// quoting. A mod from a registry other than Thunderstore carries a source line, and with
// everySource so does every mod that names a registry, for a code stored where a mod without
// one would be taken for that registry's own.
func Archive(name string, mods []Mod, everySource bool) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "profileName: %q\nmods:\n", name)
	for _, mod := range mods {
		v, ok := semver.Parse(mod.Version)
		if !ok {
			return nil, fmt.Errorf("export %s: version %q is not major.minor.patch", mod.FullName, mod.Version)
		}
		fmt.Fprintf(&b, "  - name: %s\n    version:\n", mod.FullName)
		fmt.Fprintf(&b, "      major: %d\n      minor: %d\n      patch: %d\n", v[0], v[1], v[2])
		fmt.Fprintf(&b, "    enabled: %t\n", mod.Enabled)
		if mod.Source != "" && (everySource || mod.Source != thunderstore) {
			if strings.Trim(mod.Source, "abcdefghijklmnopqrstuvwxyz") != "" {
				return nil, fmt.Errorf("export %s: registry %q is not a plain name", mod.FullName, mod.Source)
			}
			fmt.Fprintf(&b, "    source: %s\n", mod.Source)
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry, err := zw.CreateHeader(&zip.FileHeader{Name: manifestFile, Method: zip.Deflate, Modified: ArchiveTime})
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", manifestFile, err)
	}
	if _, err := io.WriteString(entry, b.String()); err != nil {
		return nil, fmt.Errorf("write %s: %w", manifestFile, err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close profile archive: %w", err)
	}
	return buf.Bytes(), nil
}
