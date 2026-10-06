package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// BepInExPack is the mod framework package. A vanilla instance receiving its first mod has
// it added to the closure automatically.
const BepInExPack = manager.BepInExPack

// installMods handles POST /instances/{id}/mods: resolve, download, place, and record a
// manifest per package. It answers 202 with a job.
func (m *Mods) installMods(w http.ResponseWriter, r *http.Request) {
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

	body, ok := decodePackageRequest(w, r)
	if !ok {
		return
	}

	audit, err := m.installAudit(r.Context(), u.ID, id, body)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	job, err := m.submitInstall(r.Context(), inst, body, u.ID, audit, nil)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// modVersionChange is one package moving between versions in audit detail. From is empty for a
// package that was not installed.
type modVersionChange struct {
	FullName string `json:"full_name"`
	From     string `json:"from,omitempty"`
	To       string `json:"to"`
}

// modInstallDetail is the audit detail of one package install.
type modInstallDetail struct {
	modVersionChange
	Source string `json:"source"`
}

// installAudit is the audit entry of an install request, recording the version it replaces. The
// registry is the one the operator named, else the one an installed copy came from, which is
// where the job resolves it.
func (m *Mods) installAudit(
	ctx context.Context, userID, instanceID string, req resolveRequest,
) (*store.AuditEntry, error) {
	from, installedFrom, installed, err := m.DB.InstanceModVersion(ctx, instanceID, req.FullName)
	if err != nil {
		return nil, fmt.Errorf("read the installed version of %s: %w", req.FullName, err)
	}
	src := req.Source
	if src == "" && installed {
		src = installedFrom.String()
	}
	return jobAudit(ctx, userID, instanceID, "instances.mods.install", modInstallDetail{
		modVersionChange: modVersionChange{FullName: req.FullName, From: from, To: req.Version},
		Source:           src,
	}), nil
}

// CheckResolvable reports whether the index can produce a closure for req, for an instance that
// does not exist yet. It computes the closure and discards it, so an unresolvable request fails
// the create call rather than a job running after the game download.
func (m *Mods) CheckResolvable(ctx context.Context, inst *store.Instance, req manager.PackageRequest) error {
	prefer, _ := source.ByName(req.Source)
	idx := m.newStoreIndex(ctx, inst.ID, prefer)
	_, resolveErr := m.planner().PlanInstall(ctx, inst, req.FullName, req.Version, idx)
	if idx.Err != nil {
		return idx.Err
	}
	//nolint:wrapcheck // preserve the resolver's typed error and message
	return resolveErr
}

// SubmitInstall submits an install on behalf of a definition chain, which follows the work
// through afterFinish and reports the job id to whoever asked for the step.
func (m *Mods) SubmitInstall(
	ctx context.Context,
	inst *store.Instance,
	req manager.PackageRequest,
	requestedBy string,
	afterFinish func(context.Context),
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts for the operation chain
	return m.installer().Submit(ctx, inst, &manager.InstallPayload{
		FullName: req.FullName, Version: req.Version, Source: req.Source, Minimum: true,
	}, "install", requestedBy, nil, afterFinish)
}

// submitInstall converts the HTTP request into a manager install submission.
func (m *Mods) submitInstall(
	ctx context.Context,
	inst *store.Instance,
	req resolveRequest,
	requestedBy string,
	audit *store.AuditEntry,
	afterFinish func(context.Context),
) (*store.Job, error) {
	//nolint:wrapcheck // preserve typed job conflicts for the HTTP response
	return m.installer().Submit(ctx, inst, &manager.InstallPayload{
		FullName: req.FullName, Version: req.Version, Source: req.Source,
	}, "install", requestedBy, audit, afterFinish)
}

// installedModView is one row of GET /instances/{id}/mods. The file manifest is not on it:
// it belongs to uninstall, runs to thousands of paths, and no screen renders it.
type installedModView struct {
	FullName string `json:"full_name"`
	// Source is the registry the installed files came from, so the UI can mark it and compare
	// an available update against the same registry rather than the other one.
	Source string `json:"source"`
	// The package's author and its own name, carried separately so a screen can render
	// "Warfare, by Therzie" rather than the ident. Read from the catalogue and never split out of
	// FullName, whose halves may each contain a hyphen; empty when the catalogue holds no row.
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	UpdateVersion string `json:"update_version"`
	IsDeprecated  bool   `json:"is_deprecated"`
	// NotIndexed is a package its own registry's last complete listing did not carry. Nothing
	// about it can be read any more, which is not the same as an author saying nothing, so it is
	// reported rather than left to look like a healthy row (Q39).
	NotIndexed  bool   `json:"not_indexed"`
	InstalledAs string `json:"installed_as"`
	Side        string `json:"side"`
	Enabled     bool   `json:"enabled"`
	// Locked holds the mod at Version: Update all skips it and no install moves it.
	Locked bool `json:"locked"`
	// IsPack is a modpack: a package whose dependencies are the mods it bundles.
	IsPack bool `json:"is_pack"`
	// Pack is the installed modpack naming this mod, PackVersion the version that pack pins,
	// and PackOverride true when the mod no longer follows the pack: it is locked, was installed
	// by hand, or sits at another version. Empty and false outside any installed modpack.
	Pack         string `json:"pack"`
	PackVersion  string `json:"pack_version"`
	PackOverride bool   `json:"pack_override"`
	InstalledAt  string `json:"installed_at"`
	FileCount    int    `json:"file_count"`
	// ConfigFileCount is how many of FileCount are under BepInEx/config/: the files an
	// uninstall leaves in place, since they hold the admin's settings.
	ConfigFileCount int `json:"config_file_count"`
	// ConfigFiles names, sorted, the config files this package placed that the configs endpoints
	// serve, as those endpoints name them. A file a plugin writes on first launch is not listed.
	ConfigFiles []string `json:"config_files"`
	// LoadStatus is this mod's load verification. Null means there is nothing to compare
	// against — no BepInEx log yet, or a package that places no plugin — and is distinct
	// from LoadNotSeen, which is an observation.
	LoadStatus *string `json:"load_status"`
	// LoadError is the loader's own line naming the failure when LoadStatus is LoadFailed, and
	// null otherwise.
	LoadError *string `json:"load_error"`
}

// Load statuses on installedModView. "failed" is a plugin the chainloader said it could not
// load (Q38); "not_seen" is a plugin it said nothing about, which stays the superset for a failure
// no failure line describes.
const (
	LoadLoaded  = "loaded"
	LoadNotSeen = "not_seen"
	LoadFailed  = "failed"
)

// pluginLoadView is the boot-level half of load verification: what BepInEx said it would
// load, what it named, and whether those two disagree.
type pluginLoadView struct {
	ObservedAt string `json:"observed_at"`
	// Declared is the count line's number, null when the run printed none.
	Declared *int `json:"declared"`
	Loaded   int  `json:"loaded"`
	// Failed is how many plugins the loader said it could not load.
	Failed int `json:"failed"`
	// Discrepancy is null when the two agree. It is reported rather than resolved: the
	// gap between them is a plugin BepInEx meant to load and never named.
	Discrepancy *string `json:"discrepancy"`
}

// listInstalledMods handles GET /instances/{id}/mods, gated on mods.list.
func (m *Mods) listInstalledMods(w http.ResponseWriter, r *http.Request) {
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

	inst, err := m.DB.InstanceByID(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	mods, err := m.DB.InstanceModsCatalogued(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	starts, err := manager.ListingStarts(r.Context(), m.DB)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	members, err := m.planner().PackMembership(r.Context(), mods)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	// A log the panel cannot read costs the load statuses and nothing else — the installed
	// list comes from the database. Failing the page here would hide the screen an admin
	// reaches for precisely when their mods are not working.
	load, err := instance.ReadPluginLoad(inst.DataDir)
	if err != nil {
		slog.WarnContext(r.Context(), "could not read the BepInEx log for load verification",
			slog.String("instance_id", id), slog.Any("error", err))
	}

	views := make([]installedModView, 0, len(mods))
	for i := range mods {
		// The join answered the update and deprecation questions for every row (Q39). Only a
		// package the installed registry's catalogue lacks costs a lookup of its own, and only
		// for its author and display name, which another registry may still carry. A miss is
		// not an error — see installedModView.Namespace.
		pkg := mods[i].Package
		if pkg == nil {
			if pkg, err = m.indexedPackage(r.Context(), mods[i].FullName, mods[i].Source, nil); err != nil {
				apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
				return
			}
		}
		view := toInstalledModView(&mods[i].InstanceMod, pkg, load)
		withPack(&view, &mods[i].InstanceMod, members)
		if _, enabled := m.Clients[mods[i].Source]; !enabled {
			view.UpdateVersion = ""
		}
		if manager.Unlisted(&mods[i], starts) {
			// The row's latest version is what the registry offered before it pulled the
			// package, so it is not an update anyone can install.
			view.NotIndexed, view.UpdateVersion = true, ""
		}
		views = append(views, view)
	}
	JSON(w, r, http.StatusOK, map[string]any{"mods": views, "plugin_load": toPluginLoadView(load)})
}

func toInstalledModView(m *store.InstanceMod, pkg *store.ModPackage, load *instance.PluginLoad) installedModView {
	var manifest []installer.ManifestEntry
	// A manifest that will not decode costs the file count and nothing else, so the row is
	// still listed: a mod the user can see and uninstall beats a 500 on the whole page.
	_ = json.Unmarshal([]byte(m.FileManifest), &manifest)
	var namespace, name string
	deprecated := false
	if pkg != nil {
		namespace, name = pkg.Namespace, pkg.Name
		deprecated = pkg.Source == m.Source && pkg.IsDeprecated
	}
	// A disabled mod is not meant to load, so there is nothing to verify (Q37).
	var status, loadErr *string
	if m.Enabled {
		status, loadErr = loadStatus(m.FullName, manifest, load)
	}
	configs, configFiles := 0, []string{}
	for _, e := range manifest {
		if !installer.UserConfig(e.Path) {
			continue
		}
		configs++
		// The configs endpoints address only a flat .cfg directly under the config directory.
		file := strings.TrimPrefix(e.Path, configDir+"/")
		if !strings.Contains(file, "/") && strings.HasSuffix(file, ".cfg") {
			configFiles = append(configFiles, file)
		}
	}
	slices.Sort(configFiles)
	return installedModView{
		Source: m.Source.String(), IsDeprecated: deprecated,
		FullName: m.FullName, Namespace: namespace, Name: name,
		Version: m.Version, UpdateVersion: manager.ModUpdateVersion(m, pkg), InstalledAs: m.InstalledAs,
		Side: m.Side, Enabled: m.Enabled, Locked: m.Locked, IsPack: isPack(pkg), InstalledAt: m.InstalledAt,
		FileCount: len(manifest), ConfigFileCount: configs, ConfigFiles: configFiles,
		LoadStatus: status, LoadError: loadErr,
	}
}

// withPack fills a view's modpack fields from the installed modpacks' membership.
func withPack(view *installedModView, row *store.InstanceMod, members map[string]packMember) {
	member, ok := members[row.FullName]
	if !ok {
		return
	}
	view.Pack, view.PackVersion = member.Pack, member.Version
	view.PackOverride = !followsPack(row, member.Version)
}

// loadStatus reports whether one mod loaded, and the loader's line when it said it could not.
// Null means no answer: the package places no plugin, or there is no chainloader run to read yet.
// An admin who has not restarted since installing must not be told the mod is not loading.
//
// A failure line outranks a load line: a plugin whose load threw was named `Loading [...]` on
// its way to the exception.
func loadStatus(
	fullName string, manifest []installer.ManifestEntry, load *instance.PluginLoad,
) (status, reason *string) {
	if load == nil {
		return nil, nil
	}
	paths := installer.Paths(manifest)
	if !instance.IsPlugin(paths) {
		return nil, nil
	}
	if why, failed := load.FailedFor(fullName, paths); failed {
		s := LoadFailed
		return &s, &why
	}
	s := LoadNotSeen
	if load.Loaded(fullName, paths) {
		s = LoadLoaded
	}
	return &s, nil
}

func toPluginLoadView(load *instance.PluginLoad) *pluginLoadView {
	if load == nil {
		return nil
	}
	view := pluginLoadView{
		ObservedAt: load.ObservedAt.UTC().Format(time.RFC3339),
		Loaded:     load.LoadedCount(),
		Failed:     len(load.Failed),
	}
	if load.Declared >= 0 {
		declared := load.Declared
		view.Declared = &declared
	}
	if d := load.Discrepancy(); d != "" {
		view.Discrepancy = &d
	}
	return &view
}

// decodePackageRequest reads the {full_name, version, source?} body both resolve and install
// take, answering 422 with the field errors if a required one is missing or source names no
// configured registry. An unrecognised registry is rejected rather than ignored: silently
// installing from the other one is the wrong bytes, not a near miss (B14).
func decodePackageRequest(w http.ResponseWriter, r *http.Request) (resolveRequest, bool) {
	var body resolveRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	var val apierr.Validation
	if strings.TrimSpace(body.FullName) == "" {
		val.Add("full_name", apierr.FieldRequired, "full_name is required.")
	}
	if strings.TrimSpace(body.Version) == "" {
		val.Add("version", apierr.FieldRequired, "version is required.")
	}
	if name := strings.TrimSpace(body.Source); name != "" {
		if _, ok := source.ByName(name); !ok {
			val.Add("source", apierr.FieldInvalid, "source must name a configured mod registry.")
		}
	}
	if err := val.Err(); err != nil {
		apierr.Write(w, r, err)
		return body, false
	}
	return body, true
}
