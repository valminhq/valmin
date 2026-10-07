package api

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

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

func (h *Instances) configRoutes(rt *routeTable) {
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

	root, err := instance.OpenConfigDir(inst.DataDir)
	if errors.Is(err, fs.ErrNotExist) {
		JSON(w, r, http.StatusOK, configListView{Items: []configFileView{}, Note: noConfigYet})
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		item, skip, err := configListEntry(root, e.Name())
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		if skip {
			continue
		}
		view.Items = append(view.Items, item)
	}
	sort.Slice(view.Items, func(i, j int) bool { return view.Items[i].File < view.Items[j].File })
	if len(view.Items) == 0 && view.Note == "" {
		view.Note = noConfigYet
	}
	JSON(w, r, http.StatusOK, view)
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
		dir, name, ok := resolveConfig(w, r, inst)
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
		dir, name, ok := resolveConfig(w, r, inst)
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
	dir, name, ok := resolveConfig(w, r, inst)
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
	dir, name, ok := resolveConfig(w, r, inst)
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

// loadConfig resolves and reads the file named by {file} for a read handler.
func (h *Instances) loadConfig(w http.ResponseWriter, r *http.Request, id string) (file string, raw []byte, ok bool) {
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return "", nil, false
	}
	dir, name, ok := resolveConfig(w, r, inst)
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

// resolveConfig validates {file} and opens the instance's config directory for it. A name
// that is not a plain `.cfg` basename is a 404, never an error naming what it refused (B5, D2,
// D13), and so is a missing directory. The caller closes dir.
func resolveConfig(w http.ResponseWriter, r *http.Request, inst *store.Instance) (dir *os.Root, name string, ok bool) {
	name = r.PathValue("file")
	if err := configName(name); err != nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, "", false
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
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
// file atomically, mark the instance as needing a restart, and audit it. raw is whether the
// caller replaced the whole file rather than patching keys.
func (h *Instances) saveConfig(
	w http.ResponseWriter, r *http.Request, u *store.User, inst *store.Instance,
	dir *os.Root, name string, current, next []byte, raw bool,
) bool {
	if err := fsutil.WriteFileAtomicIn(dir, name+backupSuffix, current); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return false
	}
	// Written once and then left alone, so it holds the file as it was before the panel's
	// first write rather than as it was one save ago.
	if _, err := dir.Lstat(name + originalSuffix); errors.Is(err, fs.ErrNotExist) {
		if err := fsutil.WriteFileAtomicIn(dir, name+originalSuffix, current); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return false
		}
	} else if err != nil {
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
	if err := h.DB.SetRestartRequired(r.Context(), inst.ID); err != nil {
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
