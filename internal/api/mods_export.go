package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/mods/profile"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// Side tags an export reads (04 §2's CHECK constraint). store.SideUnknown covers the fourth.
const (
	sideServerOnly     = "server_only"
	sideClientRequired = "client_required"
	sideClientOptional = "client_optional"
)

// Why a package is in the export. An exclusion carries its side tag as the reason instead, or
// reasonDisabled for a mod the server does not load (Q37).
const (
	reasonTagged     = "tagged"
	reasonDependency = "dependency"
	reasonDisabled   = "disabled"
)

// exportEntry is one line of the manifest, or one line of what was left out of it.
type exportEntry struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	Source   string `json:"source"`
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

// exportClientManifest is GET /instances/{id}/mods/export: the client-side mod list derived
// from the side tags an admin set and from nothing else. Gated on mods.list, the same
// capability as the installed list it is computed from.
func (m *Mods) exportClientManifest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "r2z" {
		apierr.Write(w, r, apierr.New(errcode.InvalidParameter).With("parameter", "format"))
		return
	}
	inst, preview, ok := m.loadExport(w, r, id)
	if !ok {
		return
	}
	if format == "" {
		JSON(w, r, http.StatusOK, preview)
		return
	}
	archive, ok := exportArchive(w, r, preview, false)
	if !ok {
		return
	}
	name := profileFileName(inst.Name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, profile.ArchiveTime, bytes.NewReader(archive))
}

// exportCodeView is a profile code: the code a mod manager imports, the registry that stores
// it, and when it was uploaded.
type exportCodeView struct {
	Code      string    `json:"code"`
	Registry  string    `json:"registry"`
	CreatedAt time.Time `json:"created_at"`
}

// currentCodeView is GET /instances/{id}/mods/export/code: the stored profile code while the
// server's mods still match it, else null.
type currentCodeView struct {
	Current *exportCodeView `json:"current"`
}

// currentClientCode is GET /instances/{id}/mods/export/code: the last profile code uploaded for
// this server, while it still carries the mods players need. It never uploads.
func (m *Mods) currentClientCode(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	if _, ok := m.mustLoadInstance(w, r, id); !ok {
		return
	}
	code, err := m.currentModpackCode(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	var view currentCodeView
	if code != nil {
		view.Current = &exportCodeView{Code: code.Code, Registry: code.Registry, CreatedAt: code.CreatedAt}
	}
	JSON(w, r, http.StatusOK, view)
}

// currentModpackCode returns the server's stored profile code when the client mod list it
// carries is still the one the server's mods give, and nil otherwise.
func (m *Mods) currentModpackCode(ctx context.Context, instanceID string) (*store.ModpackCode, error) {
	code, err := m.DB.ModpackCode(ctx, instanceID)
	if err != nil || code == nil {
		return nil, err
	}
	installed, err := m.DB.InstanceMods(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read mods of instance %s: %w", instanceID, err)
	}
	preview, err := m.buildExport(ctx, "", installed)
	if err != nil {
		return nil, err
	}
	if len(preview.Conflicts) > 0 || manifestDigest(preview.Mods) != code.ManifestSHA256 {
		return nil, nil
	}
	return code, nil
}

// manifestDigest is a hex SHA-256 over the registry, name and version of each mod in a client
// mod list, independent of their order.
func manifestDigest(mods []exportEntry) string {
	lines := make([]string, len(mods))
	for i, e := range mods {
		lines[i] = e.Source + "/" + e.FullName + "@" + e.Version
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// exportClientCode is POST /instances/{id}/mods/export/code: it uploads the client mod list to
// a registry, stores the profile code r2modman and Gale import as the server's current one,
// and returns it. It needs the same actions as the file export, and each user may upload a few
// times a minute; a request refused before the upload does not count.
func (m *Mods) exportClientCode(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !m.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !m.Authz.Can(r.Context(), u, authz.ModsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	_, preview, ok := m.loadExport(w, r, id)
	if !ok {
		return
	}
	registry, err := m.uploadRegistry(r.Context(), preview.Mods)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	archive, ok := exportArchive(w, r, preview, registry == source.Hexium)
	if !ok {
		return
	}
	host, ok := m.Profiles[registry]
	if !ok {
		apierr.Write(w, r, apierr.New(errcode.Unavailable).Msg("This registry does not store profile codes."))
		return
	}
	if allowed, retry := m.profileCodeLimit.Allow(u.ID); !allowed {
		middleware.RefuseRateLimited(w, r, retry)
		return
	}
	code, err := host.Upload(r.Context(), archive)
	switch {
	case errors.Is(err, profile.ErrTooLarge):
		apierr.Write(w, r, apierr.New(errcode.PayloadTooLarge).
			Msg("This modpack is larger than the registry accepts."))
		return
	case err != nil:
		apierr.Write(w, r, apierr.New(errcode.Unavailable).
			Msg("The registry did not accept the modpack. Try again in a moment.").Wrap(err))
		return
	}
	saved := &store.ModpackCode{
		InstanceID: id, Code: code, Registry: registry.String(),
		ManifestSHA256: manifestDigest(preview.Mods), CreatedAt: time.Now().UTC(),
	}
	// The code exists once uploaded, so it is kept even if the caller has gone.
	if err := m.DB.SaveModpackCode(context.WithoutCancel(r.Context()), saved); err != nil {
		slog.ErrorContext(r.Context(), "could not store the profile code",
			slog.String("instance_id", id), slog.Any("error", err))
	}
	JSON(w, r, http.StatusOK, exportCodeView{Code: code, Registry: saved.Registry, CreatedAt: saved.CreatedAt})
}

// uploadRegistry picks where a profile code is stored: Thunderstore, which r2modman reads, unless
// a mod exists only in Hexium and Hexium is enabled.
func (m *Mods) uploadRegistry(ctx context.Context, mods []exportEntry) (source.Source, error) {
	if _, ok := m.Profiles[source.Hexium]; !ok {
		return source.Thunderstore, nil
	}
	for _, mod := range mods {
		if mod.Source != source.Hexium.String() {
			continue
		}
		_, _, onThunderstore, err := m.DB.ModVersionDependenciesFrom(
			ctx, mod.FullName, mod.Version, source.Source{}, []source.Source{source.Thunderstore})
		if err != nil {
			return source.Source{}, fmt.Errorf("look up %s %s: %w", mod.FullName, mod.Version, err)
		}
		if !onThunderstore {
			return source.Hexium, nil
		}
	}
	return source.Thunderstore, nil
}

// loadExport computes an instance's client export. It writes the response and reports false when
// it cannot.
func (m *Mods) loadExport(w http.ResponseWriter, r *http.Request, id string) (*store.Instance, *exportPreview, bool) {
	inst, ok := m.mustLoadInstance(w, r, id)
	if !ok {
		return nil, nil, false
	}
	installed, err := m.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, nil, false
	}
	preview, err := m.buildExport(r.Context(), inst.Name, installed)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, nil, false
	}
	return inst, preview, true
}

// exportArchive renders the preview as a profile archive, naming every mod's registry when
// everySource is set. A conflicting closure would be a client that cannot start, so it is
// refused while the preview still reports why.
func exportArchive(w http.ResponseWriter, r *http.Request, p *exportPreview, everySource bool) ([]byte, bool) {
	if len(p.Conflicts) > 0 {
		apierr.Write(w, r, apierr.New(errcode.ModConflict).
			With("conflicts", fmt.Sprintf("%d", len(p.Conflicts))))
		return nil, false
	}
	mods := make([]profile.Mod, len(p.Mods))
	for i, e := range p.Mods {
		mods[i] = profile.Mod{FullName: e.FullName, Version: e.Version, Source: e.Source, Enabled: true}
	}
	archive, err := profile.Archive(p.ProfileName, mods, everySource)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	return archive, true
}

// buildExport walks 04 §3's membership rules: the tagged seed, then its transitive closure at
// the versions this instance has installed, then everything left over as an exclusion. The
// panel classifies nothing on its own (03 §5.6) — an untagged package is reported, not
// guessed at.
func (m *Mods) buildExport(ctx context.Context, name string, installed []store.InstanceMod) (*exportPreview, error) {
	byName := make(map[string]*store.InstanceMod, len(installed))
	for i := range installed {
		byName[installed[i].FullName] = &installed[i]
	}

	included := map[string]exportEntry{}
	queue := make([]string, 0, len(installed))
	for i := range installed {
		mod := &installed[i]
		// A disabled mod does not run on the server, so players do not need it (Q37).
		if mod.Enabled && (mod.Side == sideClientRequired || mod.Side == sideClientOptional) {
			included[mod.FullName] = exportEntry{
				FullName: mod.FullName, Version: mod.Version, Source: mod.Source.String(),
				Side: mod.Side, Reason: reasonTagged,
			}
			queue = append(queue, mod.FullName)
		}
	}

	conflicts, err := m.walkClosure(ctx, byName, included, queue)
	if err != nil {
		return nil, err
	}

	preview := &exportPreview{
		ProfileName: name,
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
		reason := mod.Side
		if !mod.Enabled {
			reason = reasonDisabled
		}
		preview.Excluded = append(preview.Excluded, exportEntry{
			FullName: mod.FullName, Version: mod.Version, Source: mod.Source.String(),
			Side: mod.Side, Reason: reason,
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
	ctx context.Context,
	byName map[string]*store.InstanceMod,
	included map[string]exportEntry,
	queue []string,
) ([]exportConflict, error) {
	conflicts := []exportConflict{}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		entry := included[parent]
		var prefer source.Source
		if row, seen := byName[parent]; seen {
			prefer = row.Source
		}
		deps, _, ok, err := m.DB.ModVersionDependencies(ctx, parent, entry.Version, prefer)
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
	side, src := store.SideUnknown, ""
	if mod, ok := byName[fullName]; ok {
		// The installed version is what this server actually runs, and parity is the whole
		// point of the export (03 §5.6). The dependency string only pins a floor.
		version, side, src = mod.Version, mod.Side, mod.Source.String()
	}
	if side == sideServerOnly {
		return &exportConflict{
			FullName: fullName, Version: version, RequiredBy: parent, Side: side,
		}, ""
	}
	if entry, seen := included[fullName]; seen {
		// Why it is in the profile, not how it was labelled: since a tag carries down a
		// closure (ADR-175), a package can be both tagged and dragged in, and what the
		// operator needs to see is that they did not choose it.
		if entry.Reason == reasonTagged {
			entry.Reason = reasonDependency
			included[fullName] = entry
		}
		return nil, ""
	}
	included[fullName] = exportEntry{
		FullName: fullName, Version: version, Source: src, Side: side, Reason: reasonDependency,
	}
	return nil, fullName
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
