package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// noConfigYet is the empty state, composed here because the SPA holds no Valheim knowledge
// (F2). A `.cfg` is generated on the plugin's first launch (03 §9).
const noConfigYet = "No config files yet. Start the server once so its mods can write them."

// nestedConfigNote warns that a subdirectory was skipped. These endpoints address the one
// flat file per plugin that 03 §9 documents (Q46).
const nestedConfigNote = "Some settings are in subdirectories, which this screen cannot show yet."

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
}

type configListView struct {
	Items []configFileView `json:"items"`
	Note  string           `json:"note,omitempty"`
}

type configFileView struct {
	File   string `json:"file"`
	Plugin string `json:"plugin"`
	Bytes  int64  `json:"size_bytes"`
	// Dir is the file's directory relative to the game installation: the plugin config
	// directory, or empty for a file an installed mod keeps at the installation root.
	Dir string `json:"dir"`
	// InstalledMods names the installed mods the file belongs to, matched by name; empty for a
	// file no installed mod claims.
	InstalledMods []string `json:"installed_mods"`
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

// listConfigDir is the view of the config directory. A missing directory has no files.
func listConfigDir(dataDir string) (configListView, error) {
	root, err := instance.OpenConfigDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return configListView{Items: []configFileView{}}, nil
	}
	if err != nil {
		return configListView{}, err //nolint:wrapcheck // OpenConfigDir names the directory
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return configListView{}, fmt.Errorf("list config files: %w", err)
	}
	view, err := configList(root, entries)
	for i := range view.Items {
		view.Items[i].Dir = instance.ConfigDir
	}
	return view, err
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
	entries = slices.DeleteFunc(entries, func(e fs.DirEntry) bool {
		return e.IsDir() || slices.ContainsFunc(shadowing, func(c configFileView) bool { return c.File == e.Name() })
	})
	view, err := configList(root, entries)
	return view.Items, err
}

// configList is the view of the config directory's entries: its regular `.cfg` files, and the
// note when a subdirectory was skipped.
func configList(root *os.Root, entries []fs.DirEntry) (configListView, error) {
	view := configListView{Items: []configFileView{}}
	for _, e := range entries {
		if e.IsDir() {
			view.Note = nestedConfigNote
			continue
		}
		if !strings.HasSuffix(e.Name(), ".cfg") {
			continue
		}
		item, skip, err := configListEntry(root, e.Name())
		if err != nil {
			return configListView{}, err
		}
		if !skip {
			view.Items = append(view.Items, item)
		}
	}
	return view, nil
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

// configListEntry reads one directory entry already known to be a `.cfg`-suffixed name. skip is
// true for anything that turned out not to be a regular file.
func configListEntry(root *os.Root, name string) (item configFileView, skip bool, err error) {
	raw, info, err := fsutil.ReadRegularIn(root, name)
	if errors.Is(err, fsutil.ErrNotRegular) {
		return configFileView{}, true, nil
	}
	if err != nil {
		return configFileView{}, false, err //nolint:wrapcheck // fsutil names the file
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
		return nil, err //nolint:wrapcheck // names the directory
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
		dir, name, ok := h.resolveConfig(w, r, inst)
		if !ok {
			return
		}
		defer func() { _ = dir.Close() }()
		raw, info, ok := readConfigFileInfo(w, r, dir, name+suffix)
		if !ok {
			return
		}
		JSON(w, r, http.StatusOK, configCopyView{
			Schema:     modconfig.Parse(raw).Schema(r.PathValue("file")),
			CapturedAt: info.ModTime().UTC(),
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
		raw, ok := readConfigFile(w, r, dir, name+suffix)
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
	if !configEditable(w, r, inst) {
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
	if !configEditable(w, r, inst) {
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
	if !h.Authz.Can(r.Context(), u, authz.ConfigEdit, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !configEditable(w, r, inst) {
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
	if err := removeConfigCopies(dir, name); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if err := h.recordConfigDelete(r.Context(), u, inst.ID, name, by); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// removeConfigCopies removes a config file and every copy the panel keeps of it. The live file
// goes first, so a failure leaves its copies to restore it from.
func removeConfigCopies(dir *os.Root, name string) error {
	for _, suffix := range []string{"", modconfig.BackupSuffix, modconfig.OriginalSuffix, modconfig.PendingSuffix} {
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
	dir, name, ok := h.resolveConfig(w, r, inst)
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
// or the root of the game installation for a file only found there that an installed mod
// claims. A name that is not a plain `.cfg` basename is a 404, never an error naming what it
// refused (B5, D2, D13), and so is a missing directory. The caller closes dir.
func (h *Instances) resolveConfig(
	w http.ResponseWriter, r *http.Request, inst *store.Instance,
) (dir *os.Root, name string, ok bool) {
	name = r.PathValue("file")
	if err := configName(name); err != nil {
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

// configDirFor opens the directory holding the named config, wrapping fs.ErrNotExist when
// neither directory has it.
func (h *Instances) configDirFor(ctx context.Context, inst *store.Instance, name string) (*os.Root, error) {
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err == nil {
		if _, err = dir.Lstat(name); err == nil {
			return dir, nil
		}
		_ = dir.Close()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err //nolint:wrapcheck // OpenConfigDir and Lstat name the path
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

// configName accepts a plain `.cfg` basename with no separator.
func configName(file string) error {
	if file == "" || file != filepath.Base(file) || !strings.HasSuffix(file, ".cfg") {
		return fmt.Errorf("config file %q is not a plain .cfg name", file)
	}
	if strings.ContainsAny(file, `/\`) || file == "." || file == ".." {
		return fmt.Errorf("config file %q is not a plain .cfg name", file)
	}
	return nil
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

// configEditable gates both write paths to a stopped or running server. A write to a running
// one also leaves a pending copy, settled after the server stops, because a plugin may save
// the values it loaded over the file at shutdown.
func configEditable(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if st := instance.State(inst.State); st == instance.StateStopped || st == instance.StateRunning {
		return true
	}
	apierr.Write(w, r, apierr.New(errcode.InvalidState).With("state", inst.State))
	return false
}

// maxAuditedConfigChanges caps the settings one config write lists in its audit entry.
const maxAuditedConfigChanges = 50

// configAuditDetail is the detail of a config write: the file, its new size and which settings
// changed, cut to maxAuditedConfigChanges with truncated set when there were more.
func configAuditDetail(file string, current, next []byte, raw bool) string {
	diff := modconfig.Diff(current, next)
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

// saveConfig is the one write both paths go through: back the current bytes up, replace the
// file atomically, mark the instance as pending a restart, and audit it. raw is whether the
// caller replaced the whole file rather than patching keys.
func (h *Instances) saveConfig(
	w http.ResponseWriter, r *http.Request, u *store.User, inst *store.Instance,
	dir *os.Root, name string, current, next []byte, raw bool,
) bool {
	if err := modconfig.KeepCopies(dir, name, current); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	if err := markPending(dir, name, next, instance.State(inst.State) == instance.StateRunning); err != nil {
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
	return true
}

// markPending keeps the pending copy of a write to a running server, and drops a stale one on
// a write to a stopped server, whose file is now the operator's latest.
func markPending(dir *os.Root, name string, next []byte, running bool) error {
	pending := name + modconfig.PendingSuffix
	if running {
		if err := fsutil.WriteFileAtomicIn(dir, pending, next); err != nil {
			return fmt.Errorf("write %s: %w", pending, err)
		}
		return nil
	}
	if err := dir.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", pending, err)
	}
	return nil
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
