package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/semver"
	"github.com/valminhq/valmin/internal/store"
)

// Side tags an export reads (04 §2's CHECK constraint). store.SideUnknown covers the fourth.
const (
	sideServerOnly     = "server_only"
	sideClientRequired = "client_required"
	sideClientOptional = "client_optional"
)

// Why a package is in the export. An exclusion carries its side tag as the reason instead.
const (
	reasonTagged     = "tagged"
	reasonDependency = "dependency"
)

// exportEntry is one line of the manifest, or one line of what was left out of it.
type exportEntry struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	Side     string `json:"side"`
	Reason   string `json:"reason"`
}

// exportConflict is a closure that cannot be handed to a client as it stands: a package
// clients need, that either the admin marked server-only or the index cannot describe.
type exportConflict struct {
	FullName   string `json:"full_name"`
	Version    string `json:"version"`
	RequiredBy string `json:"required_by"`
	Side       string `json:"side"`
}

type exportPreview struct {
	ProfileName string           `json:"profile_name"`
	Mods        []exportEntry    `json:"mods"`
	Excluded    []exportEntry    `json:"excluded"`
	Conflicts   []exportConflict `json:"conflicts"`
}

// exportZipTime is every entry's timestamp in the archive, so two exports of unchanged input
// are byte-identical (04 §3). The zip header has nowhere to omit a time.
var exportZipTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// exportClientManifest is GET /instances/{id}/mods/export (04 §3): the client-side mod list
// of 03 §5.6, derived from the side tags an admin set and from nothing else. Gated on
// mods.list, the same capability as the installed list it is computed from.
func (m *Mods) exportClientManifest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "r2z" {
		apierr.Write(w, r, apierr.New(apierr.InvalidParameter).With("parameter", "format"))
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
	installed, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	preview, err := m.buildExport(r, inst.Name, installed)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	if format == "" {
		JSON(w, r, http.StatusOK, preview)
		return
	}
	// A conflicting closure would download as a client that cannot start, so the file is
	// refused while the preview still reports why (04 §3).
	if len(preview.Conflicts) > 0 {
		apierr.Write(w, r, apierr.New(apierr.ModConflict).
			With("conflicts", fmt.Sprintf("%d", len(preview.Conflicts))))
		return
	}
	archive, err := r2zArchive(preview)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	name := profileFileName(inst.Name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, exportZipTime, bytes.NewReader(archive))
}

// buildExport walks 04 §3's membership rules: the tagged seed, then its transitive closure at
// the versions this instance has installed, then everything left over as an exclusion. The
// panel classifies nothing on its own (03 §5.6) — an untagged package is reported, not
// guessed at.
func (m *Mods) buildExport(r *http.Request, profile string, installed []store.InstanceMod) (*exportPreview, error) {
	byName := make(map[string]*store.InstanceMod, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = &installed[i]
	}

	included := map[string]exportEntry{}
	queue := make([]string, 0, len(installed))
	for i := range installed {
		mod := &installed[i]
		if mod.Side == sideClientRequired || mod.Side == sideClientOptional {
			included[mod.FullName] = exportEntry{
				FullName: mod.FullName, Version: mod.Version, Side: mod.Side, Reason: reasonTagged,
			}
			queue = append(queue, mod.FullName)
		}
	}

	conflicts, err := m.walkClosure(r, byName, included, queue)
	if err != nil {
		return nil, err
	}

	preview := &exportPreview{
		ProfileName: profile,
		Mods:        make([]exportEntry, 0, len(included)),
		Excluded:    []exportEntry{},
		Conflicts:   conflicts,
	}
	for _, entry := range included {
		preview.Mods = append(preview.Mods, entry)
	}
	for i := range installed {
		mod := &installed[i]
		if _, in := included[mod.FullName]; in {
			continue
		}
		preview.Excluded = append(preview.Excluded, exportEntry{
			FullName: mod.FullName, Version: mod.Version, Side: mod.Side, Reason: mod.Side,
		})
	}
	// Sorted so repeated exports of the same input are identical, here and in the archive.
	sort.Slice(preview.Mods, func(i, j int) bool { return preview.Mods[i].FullName < preview.Mods[j].FullName })
	return preview, nil
}

// walkClosure grows included with each queued package's dependencies, reading them from the
// cached index, and returns what could not be included. queue holds the packages in included
// whose dependencies have not been read yet.
func (m *Mods) walkClosure(
	r *http.Request,
	byName map[string]*store.InstanceMod,
	included map[string]exportEntry,
	queue []string,
) ([]exportConflict, error) {
	conflicts := []exportConflict{}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		entry := included[parent]
		deps, ok, err := m.DB.ModVersionDependencies(r.Context(), parent, entry.Version)
		if err != nil {
			return nil, fmt.Errorf("read dependencies of %s: %w", parent, err)
		}
		if !ok {
			// The index cannot say what this needs, so a client built from the export would be
			// missing dependencies nobody listed. Reported, never assumed empty.
			conflicts = append(conflicts, exportConflict{
				FullName: parent, Version: entry.Version, RequiredBy: parent, Side: entry.Side,
			})
			continue
		}
		for _, dep := range deps {
			conflict, added := addDependency(byName, included, parent, dep)
			if conflict != nil {
				conflicts = append(conflicts, *conflict)
			}
			if added != "" {
				queue = append(queue, added)
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].RequiredBy != conflicts[j].RequiredBy {
			return conflicts[i].RequiredBy < conflicts[j].RequiredBy
		}
		return conflicts[i].FullName < conflicts[j].FullName
	})
	return conflicts, nil
}

// addDependency resolves one dependency string against what is installed. It returns the
// conflict it is, or the name it added to included, and never both.
func addDependency(
	byName map[string]*store.InstanceMod,
	included map[string]exportEntry,
	parent, dep string,
) (conflict *exportConflict, queued string) {
	fullName, version, ok := modresolver.ParseDependency(dep)
	if !ok {
		return &exportConflict{FullName: dep, RequiredBy: parent}, ""
	}
	side := store.SideUnknown
	if mod, ok := byName[fullName]; ok {
		// The installed version is what this server actually runs, and parity is the whole
		// point of the export (03 §5.6). The dependency string only pins a floor.
		version, side = mod.Version, mod.Side
	}
	if side == sideServerOnly {
		return &exportConflict{
			FullName: fullName, Version: version, RequiredBy: parent, Side: side,
		}, ""
	}
	if _, seen := included[fullName]; seen {
		return nil, ""
	}
	included[fullName] = exportEntry{
		FullName: fullName, Version: version, Side: side, Reason: reasonDependency,
	}
	return nil, fullName
}

// r2xManifest renders the YAML the mod managers read (03 §5.6). Written directly rather than
// through a YAML library: the document is two keys deep, and every value is either an integer
// or a Thunderstore ident, whose grammar (03 §6.2) needs no escaping. Only the profile name is
// arbitrary, and it is quoted.
func r2xManifest(p *exportPreview) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "profileName: %q\n", p.ProfileName)
	b.WriteString("mods:\n")
	for _, mod := range p.Mods {
		v, ok := semver.Parse(mod.Version)
		if !ok {
			return "", fmt.Errorf("export %s: version %q is not major.minor.patch", mod.FullName, mod.Version)
		}
		fmt.Fprintf(&b, "  - name: %s\n", mod.FullName)
		b.WriteString("    version:\n")
		fmt.Fprintf(&b, "      major: %d\n      minor: %d\n      patch: %d\n", v[0], v[1], v[2])
		b.WriteString("    enabled: true\n")
	}
	return b.String(), nil
}

// r2zArchive wraps the manifest in the zip the managers associate with (03 §5.6). It carries
// export.r2x and nothing else: no config bytes, no host paths, no panel identity (04 §3).
func r2zArchive(p *exportPreview) ([]byte, error) {
	manifest, err := r2xManifest(p)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry, err := zw.CreateHeader(&zip.FileHeader{
		Name: "export.r2x", Method: zip.Deflate, Modified: exportZipTime,
	})
	if err != nil {
		return nil, fmt.Errorf("create export.r2x: %w", err)
	}
	if _, err := entry.Write([]byte(manifest)); err != nil {
		return nil, fmt.Errorf("write export.r2x: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close export archive: %w", err)
	}
	return buf.Bytes(), nil
}

// profileFileName reduces an instance name to something a browser can save. Anything outside
// the safe set becomes a hyphen rather than being dropped, so two names cannot collapse onto
// one another silently.
func profileFileName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "profile"
	}
	return safe + ".r2z"
}
