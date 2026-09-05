package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// configDir is where BepInEx writes plugin settings, relative to server/.
const configDir = "BepInEx/config"

// noConfigYet is the empty state, composed here because the SPA holds no Valheim knowledge
// (F2). A `.cfg` is generated on the plugin's first launch (03 §9).
const noConfigYet = "No config files yet. Start the server once so its mods can write them."

// backupSuffix names the copy of the bytes a write replaced, rewritten on every write
// (03 §9 rule 5). originalSuffix names the copy taken before the panel's first write, and is
// never rewritten.
const (
	backupSuffix   = ".bak"
	originalSuffix = ".orig"
)

// nestedConfigNote warns that a subdirectory was skipped. These endpoints address the one
// flat file per plugin that 03 §9 documents (Q46).
const nestedConfigNote = "Some settings are in subdirectories, which this screen cannot show yet."

func (h *Instances) configRoutes(rt *Router) {
	rt.Handle("GET /api/v1/instances/{id}/configs", http.HandlerFunc(h.listConfigs))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.readConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/original", h.readConfigCopy(originalSuffix))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/previous", h.readConfigCopy(backupSuffix))
	rt.Handle("PATCH /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.patchConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/raw", h.readConfigRaw(""))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/original/raw", h.readConfigRaw(originalSuffix))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/previous/raw", h.readConfigRaw(backupSuffix))
	rt.Handle("PUT /api/v1/instances/{id}/configs/{file}/raw", http.HandlerFunc(h.writeConfigRaw))
}

type configListView struct {
	Items []configFileView `json:"items"`
	Note  string           `json:"note,omitempty"`
}

type configFileView struct {
	File   string `json:"file"`
	Plugin string `json:"plugin"`
	Bytes  int64  `json:"size_bytes"`
}

// listConfigs handles GET /instances/{id}/configs.
func (h *Instances) listConfigs(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}

	dir := filepath.Join(serverDir(inst), filepath.FromSlash(configDir))
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		JSON(w, r, http.StatusOK, configListView{Items: []configFileView{}, Note: noConfigYet})
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	view := configListView{Items: []configFileView{}}
	for _, e := range entries {
		if e.IsDir() {
			view.Note = nestedConfigNote
			continue
		}
		if !strings.HasSuffix(e.Name(), ".cfg") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		// The plugin name comes from the file's own header, the only link it carries.
		//nolint:gosec // dir is the instance's own config directory and e.Name() came from it
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		view.Items = append(view.Items, configFileView{
			File:   e.Name(),
			Plugin: modconfig.Parse(raw).Schema(e.Name()).Plugin,
			Bytes:  info.Size(),
		})
	}
	sort.Slice(view.Items, func(i, j int) bool { return view.Items[i].File < view.Items[j].File })
	if len(view.Items) == 0 && view.Note == "" {
		view.Note = noConfigYet
	}
	JSON(w, r, http.StatusOK, view)
}

// readConfig handles GET /instances/{id}/configs/{file}, serving 04 §3's typed schema.
func (h *Instances) readConfig(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
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
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.ConfigRead, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}
		path, ok := resolveConfig(w, r, inst)
		if !ok {
			return
		}
		info, err := os.Stat(path + suffix) //nolint:gosec // path is validated by configPath
		if os.IsNotExist(err) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		raw, ok := readConfigFile(w, r, path+suffix)
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
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !h.Authz.Can(r.Context(), u, authz.ConfigRaw, id) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
		inst, ok := h.mustLoadInstance(w, r, id)
		if !ok {
			return
		}
		path, ok := resolveConfig(w, r, inst)
		if !ok {
			return
		}
		raw, ok := readConfigFile(w, r, path+suffix)
		if !ok {
			return
		}
		// The ETag of the bytes actually served, which for a copy does not match the live file.
		w.Header().Set("ETag", listETag(raw))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigEdit, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !stoppedForConfigEdit(w, r, inst) {
		return
	}
	path, ok := resolveConfig(w, r, inst)
	if !ok {
		return
	}
	current, ok := readConfigFile(w, r, path)
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
	if !h.saveConfig(w, r, u, inst, path, current, next) {
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.ConfigRaw, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !stoppedForConfigEdit(w, r, inst) {
		return
	}
	path, ok := resolveConfig(w, r, inst)
	if !ok {
		return
	}
	current, ok := readConfigFile(w, r, path)
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

	if !h.saveConfig(w, r, u, inst, path, current, next) {
		return
	}
	w.Header().Set("ETag", listETag(next))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(next)
}

// loadConfig resolves and reads the file named by {file} for a read handler.
func (h *Instances) loadConfig(w http.ResponseWriter, r *http.Request, id string) (file string, raw []byte, ok bool) {
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return "", nil, false
	}
	path, ok := resolveConfig(w, r, inst)
	if !ok {
		return "", nil, false
	}
	raw, ok = readConfigFile(w, r, path)
	if !ok {
		return "", nil, false
	}
	return r.PathValue("file"), raw, true
}

// resolveConfig turns {file} into a path inside the instance's config directory. A name that
// escapes the directory is a 404, never an error naming what it refused (B5, D2, D13).
func resolveConfig(w http.ResponseWriter, r *http.Request, inst *store.Instance) (string, bool) {
	path, err := configPath(inst, r.PathValue("file"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return "", false
	}
	return path, true
}

// configPath validates a config file name and joins it. The name must be a plain `.cfg`
// basename with no separator; the prefix check is a second guard on the joined path (B5).
func configPath(inst *store.Instance, file string) (string, error) {
	if file == "" || file != filepath.Base(file) || !strings.HasSuffix(file, ".cfg") {
		return "", fmt.Errorf("config file %q is not a plain .cfg name", file)
	}
	if strings.ContainsAny(file, `/\`) || file == "." || file == ".." {
		return "", fmt.Errorf("config file %q is not a plain .cfg name", file)
	}
	root := filepath.Join(serverDir(inst), filepath.FromSlash(configDir))
	joined := filepath.Join(root, file)
	if !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("config file %q resolves outside %s", file, root)
	}
	return joined, nil
}

// readRawBody reads the body of a raw PUT, which is the file's text itself rather than a JSON
// envelope (04 §3). The chain caps the size, so an oversized body surfaces as 413.
func readRawBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "text/plain" {
			apierr.Write(w, r, apierr.New(apierr.UnsupportedMediaType).With("content_type", ct))
			return nil, false
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Write(w, r, apierr.New(apierr.PayloadTooLarge).With("limit_bytes", tooLarge.Limit).Wrap(err))
			return nil, false
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	return body, true
}

// readConfigFile reads a config, reporting a missing one as 404 rather than 500.
func readConfigFile(w http.ResponseWriter, r *http.Request, path string) ([]byte, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is validated by configPath
	if os.IsNotExist(err) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, false
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, false
	}
	return raw, true
}

// stoppedForConfigEdit gates both write paths: BepInEx may write a plugin's settings back at
// shutdown, overwriting an edit made while the server ran (ADR-012, 12 §3.2).
func stoppedForConfigEdit(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return false
	}
	return true
}

// saveConfig is the one write both paths go through: back the current bytes up, replace the
// file atomically, mark the instance as needing a restart, and audit it.
func (h *Instances) saveConfig(
	w http.ResponseWriter, r *http.Request, u *store.User, inst *store.Instance,
	path string, current, next []byte,
) bool {
	if err := fsutil.WriteFileAtomic(path+backupSuffix, current); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	// Written once and then left alone, so it holds the file as it was before the panel's
	// first write rather than as it was one save ago.
	//nolint:gosec // path is validated by configPath
	if _, err := os.Stat(path + originalSuffix); os.IsNotExist(err) {
		if err := fsutil.WriteFileAtomic(path+originalSuffix, current); err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return false
		}
	} else if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if err := fsutil.WriteFileAtomic(path, next); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if err := h.DB.SetRestartRequired(r.Context(), inst.ID); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	if err := h.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: inst.ID, Action: "instances.configs.write",
		Detail: fmt.Sprintf("%s, %d bytes", filepath.Base(path), len(next)),
	}); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	return true
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
