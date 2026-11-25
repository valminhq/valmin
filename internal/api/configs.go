package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// noConfigYet is the empty state, composed here because the SPA holds no Valheim knowledge
// (F2). A `.cfg` is generated on the plugin's first launch (03 §9).
const noConfigYet = "No config files yet. Start the server once so its mods can write them."

// skippedConfigNote says the listing left files out for one of its limits.
const skippedConfigNote = "Some files are not listed: larger than 1 MiB, more than 8 folders deep, or past the first 5,000."

// isSettingsFile reports whether name has settings the typed endpoints serve, rather than
// being text only.
func isSettingsFile(name string) bool {
	return strings.HasSuffix(name, ".cfg")
}

func (h *Instances) configRoutes(rt *routeTable) {
	rt.Handle("GET /api/v1/instances/{id}/configs", http.HandlerFunc(h.listConfigs))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.readConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/original", h.readConfigCopy(modconfig.OriginalSuffix))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/previous", h.readConfigCopy(modconfig.BackupSuffix))
	rt.Handle("PATCH /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.patchConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/raw", h.readConfigRaw(""))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/original/raw", h.readConfigRaw(modconfig.OriginalSuffix))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/previous/raw", h.readConfigRaw(modconfig.BackupSuffix))
	rt.Handle("PUT /api/v1/instances/{id}/configs/{file}/raw", http.HandlerFunc(h.writeConfigRaw))
	rt.Handle("DELETE /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.deleteConfig))
	h.configHistoryRoutes(rt)
}

type configListView struct {
	Items []configFileView `json:"items"`
	Note  string           `json:"note,omitempty"`
}

type configFileView struct {
	// File is the file's path relative to Dir, slash-separated.
	File   string `json:"file"`
	Plugin string `json:"plugin"`
	Bytes  int64  `json:"size_bytes"`
	// Dir is the file's directory relative to the game installation: the plugin config
	// directory, or empty for a file an installed mod keeps at the installation root.
	Dir string `json:"dir"`
	// InstalledMods names the installed mods the file belongs to, matched by name; empty for a
	// file no installed mod claims.
	InstalledMods []string `json:"installed_mods"`
	// Portable is whether template codes carry the file's settings once the panel has changed
	// them: a plain .cfg directly in the config directory, other than the RCON plugin's.
	Portable bool `json:"portable"`
	// ModReplacedEdits names the mod update whose new version of the file replaced edited
	// bytes, while that is still the file's newest version; null otherwise.
	ModReplacedEdits *modReplacedView `json:"mod_replaced_edits"`
}

// listConfigs handles GET /instances/{id}/configs.
func (h *Instances) listConfigs(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}

	view, err := listConfigDir(inst.DataDir)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	rootItems, err := listRootConfigs(inst.DataDir, view.Items)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	view.Items = append(view.Items, rootItems...)
	if !h.withInstalledMods(w, r, id, view.Items) {
		return
	}
	if err := h.withModReplaced(r.Context(), id, view.Items); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	// A root file is listed only when an installed mod claims it.
	view.Items = slices.DeleteFunc(view.Items, func(item configFileView) bool {
		return item.Dir == "" && len(item.InstalledMods) == 0
	})
	sort.Slice(view.Items, func(i, j int) bool { return view.Items[i].File < view.Items[j].File })
	if len(view.Items) == 0 && view.Note == "" {
		view.Note = noConfigYet
	}
	JSON(w, r, http.StatusOK, view)
}

// listConfigDir is the view of the config tree instance.ConfigTree lists, with the plugin each
// `.cfg` header names. A missing directory has no files.
func listConfigDir(dataDir string) (configListView, error) {
	view := configListView{Items: []configFileView{}}
	root, err := instance.OpenConfigDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return view, nil
	}
	if err != nil {
		return configListView{}, err
	}
	defer func() { _ = root.Close() }()
	listing, err := instance.ConfigTree(root)
	if err != nil {
		return configListView{}, err
	}
	if listing.Truncated {
		view.Note = skippedConfigNote
	}
	for _, f := range listing.Files {
		item := configFileView{File: f.Path, Bytes: f.Size}
		if isSettingsFile(f.Path) {
			var skip bool
			item, skip, err = configListEntry(root, f.Path)
			if skip || errors.Is(err, fs.ErrNotExist) || err != nil && isLink(root, f.Path) {
				continue // gone, or a link changed since the walk
			}
			if err != nil {
				return configListView{}, err
			}
		}
		item.Dir = instance.ConfigDir
		item.Portable = control.CarriedConfig(f.Path)
		view.Items = append(view.Items, item)
	}
	return view, nil
}

// isLink reports whether name in root is a symlink.
func isLink(root *os.Root, name string) bool {
	info, err := root.Lstat(name)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// listRootConfigs is the `.cfg` files at the root of the game installation, leaving out any
// name the config directory already has.
func listRootConfigs(dataDir string, shadowing []configFileView) ([]configFileView, error) {
	root, err := os.OpenRoot(instance.ServerDir(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open server directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("list server directory: %w", err)
	}
	var items []configFileView
	for _, e := range entries {
		if e.IsDir() || !isSettingsFile(e.Name()) ||
			slices.ContainsFunc(shadowing, func(c configFileView) bool { return c.File == e.Name() }) {
			continue
		}
		item, skip, err := configListEntry(root, e.Name())
		if err != nil {
			return nil, err
		}
		if !skip {
			items = append(items, item)
		}
	}
	return items, nil
}

// withInstalledMods fills each item's InstalledMods. It writes the response and reports false when
// the installed mods cannot be read.
func (h *Instances) withInstalledMods(w http.ResponseWriter, r *http.Request, id string, items []configFileView) bool {
	rows, err := h.DB.InstanceMods(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	plugins := make(map[string]string, len(items))
	for _, item := range items {
		plugins[item.File] = item.Plugin
	}
	claims := instance.ConfigClaims(installedManifests(rows), plugins)
	for i := range items {
		items[i].InstalledMods = claims[items[i].File]
		if items[i].InstalledMods == nil {
			items[i].InstalledMods = []string{}
		}
	}
	return true
}

// configListEntry reads one name instance.CheckConfigPath accepts: a `.cfg` whole, for the plugin its
// header names, any other file only for its size. skip is true for anything that turned out
// not to be a regular file.
func configListEntry(root *os.Root, name string) (item configFileView, skip bool, err error) {
	if !isSettingsFile(name) {
		f, info, err := fsutil.OpenRegularIn(root, name)
		if errors.Is(err, fsutil.ErrNotRegular) {
			return configFileView{}, true, nil
		}
		if err != nil {
			return configFileView{}, false, err
		}
		_ = f.Close()
		return configFileView{File: name, Bytes: info.Size()}, false, nil
	}
	raw, info, err := fsutil.ReadRegularIn(root, name)
	if errors.Is(err, fsutil.ErrNotRegular) {
		return configFileView{}, true, nil
	}
	if err != nil {
		return configFileView{}, false, err
	}
	// The plugin name comes from the file's own header, the only link it carries.
	return configFileView{
		File:   name,
		Plugin: modconfig.Parse(raw).Schema(name).Plugin,
		Bytes:  info.Size(),
	}, false, nil
}

// configPlugins maps each config file to the plugin its header names. A server with no config
// directory has none.
func configPlugins(dataDir string) (map[string]string, error) {
	root, err := instance.OpenConfigDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("list config files: %w", err)
	}
	plugins := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cfg") {
			continue
		}
		item, skip, err := configListEntry(root, e.Name())
		if err != nil {
			return nil, err
		}
		if !skip {
			plugins[e.Name()] = item.Plugin
		}
	}
	return plugins, nil
}

// installedManifests maps each installed package to its manifest paths.
func installedManifests(rows []store.InstanceMod) map[string][]string {
	manifests := make(map[string][]string, len(rows))
	for i := range rows {
		manifests[rows[i].FullName] = manifestPaths(rows[i].FileManifest)
	}
	return manifests
}

// manifestPaths is the paths of a stored file manifest. One that will not decode has none, so
// its package claims config files by name only.
func manifestPaths(raw string) []string {
	var manifest []installer.ManifestEntry
	_ = json.Unmarshal([]byte(raw), &manifest)
	return installer.Paths(manifest)
}

// readConfig handles GET /instances/{id}/configs/{file}, serving 04 §3's typed schema.
func (h *Instances) readConfig(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	file, raw, ok := h.loadConfig(w, r, id)
	if !ok {
		return
	}
	w.Header().Set("ETag", listETag(raw))
	JSON(w, r, http.StatusOK, modconfig.Parse(raw).Schema(file))
}

// configCopyView is 04 §3's schema plus when the copy was taken.
type configCopyView struct {
	modconfig.Schema
	CapturedAt time.Time `json:"captured_at"`
}

// readConfigCopy serves one of the two copies a write leaves behind, projected through the
// same schema as the file itself: `/original` for the `.orig` taken before the panel's first
// write, `/previous` for the `.bak` holding what the last write replaced. A file the panel
// has never written has neither copy, which is a 404.
func (h *Instances) readConfigCopy(suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := caller(w, r)
		if !ok {
			return
		}
		// Both checks inline in each closure: authorization is visible at the route (ADR-037).
		id := r.PathValue("id")
		if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}
		dir, name, ok := h.resolveSettingsConfig(w, r, inst)
		if !ok {
			return
		}
		defer func() { _ = dir.Close() }()
		raw, at, ok := h.readKeptCopy(w, r, inst.ID, dir, name, suffix)
		if !ok {
			return
		}
		JSON(w, r, http.StatusOK, configCopyView{
			Schema:     modconfig.Parse(raw).Schema(r.PathValue("file")),
			CapturedAt: at,
		})
	}
}

// readConfigRaw serves a config file's own bytes: the live file for an empty suffix, and one
// of the kept copies for `.orig` or `.bak`. All three are gated on ConfigRaw.
func (h *Instances) readConfigRaw(suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := caller(w, r)
		if !ok {
			return
		}
		// Both checks inline in each closure: authorization is visible at the route (ADR-037).
		id := r.PathValue("id")
		if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.ConfigRaw, id) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}
		dir, name, ok := h.resolveConfig(w, r, inst)
		if !ok {
			return
		}
		defer func() { _ = dir.Close() }()
		var raw []byte
		if suffix == "" {
			raw, ok = readConfigFile(w, r, dir, name)
		} else {
			raw, _, ok = h.readKeptCopy(w, r, inst.ID, dir, name, suffix)
		}
		if !ok {
			return
		}
		// The ETag of the bytes actually served, which for a copy does not match the live file.
		w.Header().Set("ETag", listETag(raw))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		//nolint:gosec // served as text/plain, never interpreted; raw came from readConfigFile,
		// which confines the read to the config directory
		_, _ = w.Write(raw)
	}
}

// readKeptCopy reads the version `/original` or `/previous` serves, named by the suffix of the
// copy earlier releases kept: the file's baseline revision, or its newest revision that differs
// from the live file. A file with no history is served the kept copy itself. Anything else is
// a 404.
func (h *Instances) readKeptCopy(
	w http.ResponseWriter, r *http.Request, instanceID string, dir *os.Root, name, suffix string,
) (raw []byte, at time.Time, ok bool) {
	rev, tracked, err := h.keptRevision(r.Context(), instanceID, dir, name, suffix)
	switch {
	case err != nil:
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, time.Time{}, false
	case rev != nil:
		return rev.Content, rev.CreatedAt, true
	case tracked:
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, time.Time{}, false
	}
	raw, info, ok := readConfigFileInfo(w, r, dir, name+suffix)
	if !ok {
		return nil, time.Time{}, false
	}
	return raw, info.ModTime().UTC(), true
}

// keptRevision finds the revision readKeptCopy serves, and reports whether the file has any
// history at all.
func (h *Instances) keptRevision(
	ctx context.Context, instanceID string, dir *os.Root, name, suffix string,
) (*store.ConfigRevision, bool, error) {
	revs, err := h.DB.ConfigRevisions(ctx, instanceID, name)
	if err != nil || len(revs) == 0 {
		return nil, false, err
	}
	if suffix == modconfig.OriginalSuffix {
		rev, err := h.DB.ConfigBaseline(ctx, instanceID, name)
		return rev, true, err
	}
	live, _, err := fsutil.ReadRegularIn(dir, name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", name, err)
	}
	sum := sha256.Sum256(live)
	for i := range revs {
		if revs[i].SHA256 != hex.EncodeToString(sum[:]) {
			full, err := h.DB.ConfigRevision(ctx, instanceID, name, revs[i].ID)
			return full, true, err
		}
	}
	return nil, true, nil
}

// patchConfig handles PATCH /instances/{id}/configs/{file}, a body of `{"Section.Key": value}`.
// Every field is validated before any is written, so one bad value leaves the file untouched.
func (h *Instances) patchConfig(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigEdit, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !h.configEditable(w, r, inst) {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}
	dir, name, ok := h.resolveSettingsConfig(w, r, inst)
	if !ok {
		return
	}
	defer func() { _ = dir.Close() }()
	current, ok := readConfigFile(w, r, dir, name)
	if !ok {
		return
	}

	var changes map[string]any
	if err := Decode(r, &changes); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// Optional here, unlike the raw PUT: a patch names the keys it touches, so a concurrent
	// edit to other keys is not a conflict.
	if r.Header.Get("If-Match") != "" && !h.matchesCurrent(w, r, current) {
		return
	}

	doc := modconfig.Parse(current)
	if errs := doc.Apply(changes); errs != nil {
		apierr.Write(w, r, configValidation(errs).Err())
		return
	}
	next := doc.Bytes()
	if !h.saveConfig(w, r, u, inst, dir, name, current, next, false) {
		return
	}
	w.Header().Set("ETag", listETag(next))
	JSON(w, r, http.StatusOK, modconfig.Parse(next).Schema(r.PathValue("file")))
}

// writeConfigRaw handles PUT /instances/{id}/configs/{file}/raw. If-Match is required: the
// body is a full replacement, so a stale write would discard another writer's save
// (11 §1.1, G1).
func (h *Instances) writeConfigRaw(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRaw, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !h.configEditable(w, r, inst) {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}
	dir, name, ok := h.resolveConfig(w, r, inst)
	if !ok {
		return
	}
	defer func() { _ = dir.Close() }()
	current, ok := readConfigFile(w, r, dir, name)
	if !ok {
		return
	}

	next, ok := readRawBody(w, r)
	if !ok {
		return
	}
	if !h.matchesCurrent(w, r, current) {
		return
	}
	if err := checkTextFormat(name, current, next); err != nil {
		apierr.Write(w, r, err)
		return
	}
	next = matchLineEndings(current, next)
	if len(next) > instance.MaxConfigSize {
		apierr.Write(w, r, apierr.New(errcode.PayloadTooLarge).With("limit_bytes", instance.MaxConfigSize))
		return
	}

	if !h.saveConfig(w, r, u, inst, dir, name, current, next, true) {
		return
	}
	w.Header().Set("ETag", listETag(next))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(next)
}

// deleteConfig handles DELETE /instances/{id}/configs/{file}: the file and every copy the panel
// keeps of it. A file an installed mod still claims is refused with the mods that claim it unless
// allow_installed is set; that mod writes the file again, with its defaults, on its next launch.
// A `.cfg` needs config.edit; any other file is text only and needs config.raw, as writing it does.
func (h *Instances) deleteConfig(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.mayDeleteConfig(w, r, u, id) {
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !h.configEditable(w, r, inst) {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}
	allowInstalled, err := parseBoolQuery(r, "allow_installed")
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	dir, name, ok := h.resolveConfig(w, r, inst)
	if !ok {
		return
	}
	defer func() { _ = dir.Close() }()
	raw, ok := readConfigFile(w, r, dir, name)
	if !ok {
		return
	}
	by, err := h.configClaimedBy(r.Context(), id, name, raw)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if len(by) > 0 && !allowInstalled {
		apierr.Write(w, r, apierr.New(errcode.ModConflict).With("installed_mods", by))
		return
	}
	if err := h.removeConfig(r.Context(), inst.ID, dir, name, raw); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if err := h.recordConfigDelete(r.Context(), u, inst.ID, name, by); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mayDeleteConfig reports whether u may delete the file named by {file}: config.edit for a
// `.cfg`, config.raw for any other. It writes the 403 itself when not.
func (h *Instances) mayDeleteConfig(w http.ResponseWriter, r *http.Request, u *store.User, id string) bool {
	need := authz.ConfigEdit
	if !isSettingsFile(r.PathValue("file")) {
		need = authz.ConfigRaw
	}
	if !h.Authz.Can(r.Context(), u, need, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return false
	}
	return true
}

// configClaimedBy is the installed mods a config file belongs to, given its bytes.
func (h *Instances) configClaimedBy(ctx context.Context, id, name string, raw []byte) ([]string, error) {
	rows, err := h.DB.InstanceMods(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read installed mods: %w", err)
	}
	plugin := modconfig.Parse(raw).Schema(name).Plugin
	return instance.ConfigClaims(installedManifests(rows), map[string]string{name: plugin})[name], nil
}

// recordConfigDelete audits a config deletion and, when installed mods used the file, marks the
// instance as pending a restart, since they read their defaults on the next start.
func (h *Instances) recordConfigDelete(ctx context.Context, u *store.User, id, name string, by []string) error {
	if len(by) > 0 {
		if err := h.DB.SetPendingRestart(ctx, id); err != nil {
			return fmt.Errorf("mark %s as needing a restart: %w", id, err)
		}
	}
	err := h.DB.WriteAuditLog(ctx, &store.AuditEntry{
		UserID: u.ID, InstanceID: id, Action: "instances.configs.delete",
		Detail: detailJSON(map[string]any{"file": name, "installed_mods": by}), IP: clientIP(ctx),
	})
	if err != nil {
		return fmt.Errorf("audit a config deletion: %w", err)
	}
	return nil
}

// removeConfig records raw, the file's bytes, in its history, then removes the file and every
// copy the panel keeps beside it. A file too large to keep is removed without it. The live file
// goes first, so a failure leaves its copies.
func (h *Instances) removeConfig(ctx context.Context, instanceID string, dir *os.Root, name string, raw []byte) error {
	err := instance.TrackConfigBefore(ctx, h.DB, dir, instanceID, name, raw)
	if errors.Is(err, store.ErrConfigTooLarge) {
		slog.WarnContext(ctx, "config file too large to keep in its history; deleted without it",
			slog.String("instance_id", instanceID), slog.String("file", name))
	} else if err != nil {
		return err
	}
	for _, suffix := range append([]string{""}, modconfig.CopySuffixes()...) {
		if err := dir.Remove(name + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", name+suffix, err)
		}
	}
	return nil
}

// loadConfig resolves and reads the file named by {file} for a read handler.
func (h *Instances) loadConfig(w http.ResponseWriter, r *http.Request, id string) (file string, raw []byte, ok bool) {
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return "", nil, false
	}
	dir, name, ok := h.resolveSettingsConfig(w, r, inst)
	if !ok {
		return "", nil, false
	}
	defer func() { _ = dir.Close() }()
	raw, ok = readConfigFile(w, r, dir, name)
	if !ok {
		return "", nil, false
	}
	return r.PathValue("file"), raw, true
}

// resolveConfig validates {file} and opens the directory that holds it: the config directory,
// or the root of the game installation for a `.cfg` only found there that an installed mod
// claims. A name instance.CheckConfigPath refuses is a 404, never an error naming what it refused, and
// so is a missing directory. The caller closes dir.
func (h *Instances) resolveConfig(
	w http.ResponseWriter, r *http.Request, inst *store.Instance,
) (dir *os.Root, name string, ok bool) {
	name = r.PathValue("file")
	if err := instance.CheckConfigPath(name); err != nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, "", false
	}
	dir, err := h.configDirFor(r.Context(), inst, name)
	if errors.Is(err, fs.ErrNotExist) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, "", false
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, "", false
	}
	return dir, name, true
}

// resolveSettingsConfig is resolveConfig for the endpoints that serve a file's settings, which
// only a `.cfg` has. Any other name is a 404.
func (h *Instances) resolveSettingsConfig(
	w http.ResponseWriter, r *http.Request, inst *store.Instance,
) (dir *os.Root, name string, ok bool) {
	if !isSettingsFile(r.PathValue("file")) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, "", false
	}
	return h.resolveConfig(w, r, inst)
}

// configDirFor opens the directory holding the named config, wrapping fs.ErrNotExist when
// neither directory has it. Only a top-level `.cfg` is looked for at the installation root.
func (h *Instances) configDirFor(ctx context.Context, inst *store.Instance, name string) (*os.Root, error) {
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err == nil {
		if _, err = dir.Lstat(name); err == nil {
			return dir, nil
		}
		_ = dir.Close()
		if strings.Contains(name, "/") {
			// A file, a link out of the directory or a loop where a folder should be.
			err = fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	if strings.Contains(name, "/") || !isSettingsFile(name) {
		return nil, fmt.Errorf("find %s: %w", name, err)
	}
	root, err := os.OpenRoot(instance.ServerDir(inst.DataDir))
	if err != nil {
		return nil, fmt.Errorf("open server directory: %w", err)
	}
	raw, _, err := fsutil.ReadRegularIn(root, name)
	if errors.Is(err, fsutil.ErrNotRegular) {
		err = fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	var by []string
	if err == nil {
		by, err = h.configClaimedBy(ctx, inst.ID, name, raw)
	}
	if err == nil && len(by) == 0 {
		err = fmt.Errorf("%s is claimed by no installed mod: %w", name, fs.ErrNotExist)
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// readRawBody reads the body of a raw PUT, which is the file's text itself rather than a JSON
// envelope (04 §3). The chain caps the size, so an oversized body surfaces as 413.
func readRawBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "text/plain" {
			apierr.Write(w, r, apierr.New(errcode.UnsupportedMediaType).With("content_type", ct))
			return nil, false
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Write(w, r, apierr.New(errcode.PayloadTooLarge).With("limit_bytes", tooLarge.Limit).Wrap(err))
			return nil, false
		}
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	return body, true
}

// readConfigFile reads a config, reporting a missing one as 404 rather than 500.
func readConfigFile(w http.ResponseWriter, r *http.Request, dir *os.Root, name string) ([]byte, bool) {
	raw, _, ok := readConfigFileInfo(w, r, dir, name)
	return raw, ok
}

// readConfigFileInfo reads a config inside dir. A refusal, a missing file and anything but a
// regular file all read as 404.
func readConfigFileInfo(
	w http.ResponseWriter, r *http.Request, dir *os.Root, name string,
) (raw []byte, info os.FileInfo, ok bool) {
	f, info, err := fsutil.OpenRegularIn(dir, name)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, nil, false
	}
	defer func() { _ = f.Close() }()
	raw, err = io.ReadAll(f)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, nil, false
	}
	return raw, info, true
}

// configEditable gates both write paths to a stopped or running server that no job is
// changing. A write to a running one also leaves a pending copy, settled after the server
// stops, because a plugin may save the values it loaded over the file at shutdown.
func (h *Instances) configEditable(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if st := instance.State(inst.State); st != instance.StateStopped && st != instance.StateRunning {
		apierr.Write(w, r, apierr.New(errcode.InvalidState).With("state", inst.State))
		return false
	}
	// Setup restore, mod changes and definition imports hold the lock while the state stays
	// stopped, and write the same files.
	jobID, held, err := h.DB.LockHolder(r.Context(), jobs.InstanceLockKey(inst.ID))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if held {
		apierr.Write(w, r, apierr.New(errcode.JobInProgress).With("job_id", jobID))
		return false
	}
	return true
}

// maxAuditedConfigChanges caps the settings one config write lists in its audit entry.
const maxAuditedConfigChanges = 50

// configAuditDetail is the detail of a config write: the file, its new size and which settings
// changed, cut to maxAuditedConfigChanges with truncated set when there were more. A text-only
// file lists no changes.
func configAuditDetail(file string, current, next []byte, raw bool) string {
	var diff []modconfig.Change
	if isSettingsFile(file) {
		diff = modconfig.Diff(current, next)
	}
	truncated := len(diff) > maxAuditedConfigChanges
	kept := diff[:min(len(diff), maxAuditedConfigChanges)]
	changes := make([]change, 0, len(kept))
	for _, d := range kept {
		c := change{Field: d.Key, Secret: d.Secret}
		if d.From != nil {
			c.From = *d.From
		}
		if d.To != nil {
			c.To = *d.To
		}
		changes = append(changes, c)
	}
	return detailJSON(map[string]any{
		"file": file, "bytes": len(next), "raw": raw, "changes": changes, "truncated": truncated,
	})
}

// saveConfig is the one write both paths go through: record the current bytes in the file's
// history, replace the file atomically, mark the instance as pending a restart, audit it, and
// record the new bytes as the caller's edit. raw is whether the caller replaced the whole file
// rather than patching keys. New bytes equal to the current ones write nothing but the pending
// copy's state.
func (h *Instances) saveConfig(
	w http.ResponseWriter, r *http.Request, u *store.User, inst *store.Instance,
	dir *os.Root, name string, current, next []byte, raw bool,
) bool {
	running := instance.State(inst.State) == instance.StateRunning
	if bytes.Equal(current, next) {
		if err := markPending(dir, name, next, running); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
		return true
	}
	if err := instance.TrackConfigBefore(r.Context(), h.DB, dir, inst.ID, name, current); err != nil {
		apierr.Write(w, r, historyError(err))
		return false
	}
	if err := markPending(dir, name, next, running); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if err := fsutil.WriteFileAtomicIn(dir, name, next); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if err := h.DB.SetPendingRestart(r.Context(), inst.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: inst.ID, Action: "instances.configs.write",
		Detail: configAuditDetail(name, current, next, raw), IP: clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	// The file is written; a lost revision is picked up as an outside change by the next write.
	if !instance.KeepsHistory(name) {
		return true
	}
	if err := h.DB.RecordConfigRevisions(r.Context(), []store.ConfigRevision{{
		InstanceID: inst.ID, File: name, Source: store.ConfigEdit, UserID: u.ID, Content: next,
	}}); err != nil {
		slog.WarnContext(r.Context(), "config revision not recorded",
			slog.String("instance_id", inst.ID), slog.String("file", name), slog.Any("error", err))
	}
	return true
}

// historyError is the response for a write refused because the current file could not be kept
// in its history: 413 for a file too large to keep, 500 otherwise.
func historyError(err error) error {
	if errors.Is(err, store.ErrConfigTooLarge) {
		return apierr.New(errcode.PayloadTooLarge).With("limit_bytes", store.MaxConfigRevisionBytes).Wrap(err)
	}
	return apierr.New(errcode.Internal).Wrap(err)
}

// markPending keeps the pending copy of a `.cfg` write to a running server, and drops a stale
// one on a write to a stopped server, whose file is now the operator's latest. A text-only file
// has none: it is replaced whole, and settling an old copy would undo a mod's own rewrite.
func markPending(dir *os.Root, name string, next []byte, running bool) error {
	if !isSettingsFile(name) {
		return nil
	}
	pending := name + modconfig.PendingSuffix
	if running {
		if err := fsutil.WriteFileAtomicIn(dir, pending, next); err != nil {
			return fmt.Errorf("write %s: %w", pending, err)
		}
		return nil
	}
	return modconfig.DropPending(dir, name)
}

// configValidation maps the config package's own field codes onto 11 §2.4's closed registry.
func configValidation(errs []modconfig.FieldError) *apierr.Validation {
	codes := map[string]apierr.FieldCode{
		modconfig.CodeUnknownSetting: apierr.FieldUnknownSetting,
		modconfig.CodeWrongType:      apierr.FieldWrongType,
		modconfig.CodeOutOfRange:     apierr.FieldOutOfRange,
		modconfig.CodeNotAnOption:    apierr.FieldNotAnOption,
	}
	val := &apierr.Validation{}
	for _, e := range errs {
		code, ok := codes[e.Code]
		if !ok {
			code = apierr.FieldInvalid
		}
		val.Add(e.Field, code, e.Message)
	}
	return val
}
