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

// noConfigYet is the empty state as a sentence from the daemon. A `.cfg` is generated on
// the plugin's first launch (03 §9), so an empty directory is nearly always a server that
// has not been started since its mods were installed. The SPA renders this as sent: it
// holds no Valheim knowledge to compose it with (F2, ADR-110).
const noConfigYet = "No config files yet. Start the server once so its mods can write them."

// backupSuffix names the copy of the bytes a write replaced, rewritten on every write
// (03 §9 rule 5). originalSuffix names the copy taken before the panel's first write and
// never touched again — the two answer different questions, and one file cannot answer both.
const (
	backupSuffix   = ".bak"
	originalSuffix = ".orig"
)

// nestedConfigNote warns that a subdirectory was skipped. 03 §9 documents one flat file per
// plugin, which is what these endpoints address; a plugin that nests its settings would
// otherwise be silently missing from the list rather than visibly unsupported (Q46).
const nestedConfigNote = "Some settings are in subdirectories, which this screen cannot show yet."

func (h *Instances) configRoutes(rt *Router) {
	rt.Handle("GET /api/v1/instances/{id}/configs", http.HandlerFunc(h.listConfigs))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.readConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/original", http.HandlerFunc(h.readConfigOriginal))
	rt.Handle("PATCH /api/v1/instances/{id}/configs/{file}", http.HandlerFunc(h.patchConfig))
	rt.Handle("GET /api/v1/instances/{id}/configs/{file}/raw", http.HandlerFunc(h.readConfigRaw))
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
		// The plugin name comes from the file's own header, which is the only link it
		// carries; instance_mods has no column that joins to it.
		// dir is the instance's own config directory and e.Name() came from reading it.
		raw, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // see above
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

// configOriginalView is 04 §3's schema plus when the copy was taken. The timestamp is the
// point of it: a reference version is only useful to an operator who can see how old it is,
// and a plugin that regenerates its config makes this one arbitrarily stale.
type configOriginalView struct {
	modconfig.Schema
	CapturedAt time.Time `json:"captured_at"`
}

// readConfigOriginal handles GET /instances/{id}/configs/{file}/original, projecting the
// `<file>.orig` taken before the panel's first write. Gated on ConfigRead, not ConfigRaw: it
// is the same projection of the same file, so it exposes nothing the typed read does not. A
// file the panel has never written has no copy, which is a 404 and not an error.
func (h *Instances) readConfigOriginal(w http.ResponseWriter, r *http.Request) {
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
	path, ok := resolveConfig(w, r, inst)
	if !ok {
		return
	}
	info, err := os.Stat(path + originalSuffix) //nolint:gosec // path is validated by configPath
	if os.IsNotExist(err) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	raw, ok := readConfigFile(w, r, path+originalSuffix)
	if !ok {
		return
	}
	JSON(w, r, http.StatusOK, configOriginalView{
		Schema:     modconfig.Parse(raw).Schema(r.PathValue("file")),
		CapturedAt: info.ModTime().UTC(),
	})
}

// readConfigRaw handles GET /instances/{id}/configs/{file}/raw — the escape hatch, gated on
// its own capability because raw text bypasses every type and range the schema enforces.
func (h *Instances) readConfigRaw(w http.ResponseWriter, r *http.Request) {
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
	_, raw, ok := h.loadConfig(w, r, id)
	if !ok {
		return
	}
	w.Header().Set("ETag", listETag(raw))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// The chain already sets X-Content-Type-Options: nosniff, so a browser cannot rewrite
	// this declared text/plain into markup it would execute.
	_, _ = w.Write(raw) //nolint:gosec // served as text/plain with nosniff, never as markup
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
	// edit to other keys is not a conflict. A client that does send one still gets the
	// guarantee it asked for.
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

// writeConfigRaw handles PUT /instances/{id}/configs/{file}/raw. If-Match is required: this
// is a full replacement, so a second writer's save would otherwise silently discard the
// first's (11 §1.1, G1).
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

// resolveConfig turns {file} into a path inside the instance's config directory. {file} is
// user input, so a name that escapes the directory is a 404 rather than a read: an error
// naming what it refused would confirm what is outside it (B5, D2, D13).
func resolveConfig(w http.ResponseWriter, r *http.Request, inst *store.Instance) (string, bool) {
	path, err := configPath(inst, r.PathValue("file"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return "", false
	}
	return path, true
}

// configPath validates a config file name and joins it. The name must be a plain `.cfg`
// basename: 03 §9 writes one flat file per plugin, and refusing a separator outright is a
// stronger guard than normalising one away. The prefix check is the second: it holds even
// if the rules above are later loosened.
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

// readRawBody reads the body of a raw PUT. The body is the file, as 04 §3 specifies and as
// the matching GET already serves it: a config is text with newlines and quotes in it, and
// wrapping that in a JSON envelope would make the escape hatch the least direct route to the
// bytes. The chain caps the size, so an oversized body surfaces as 413 rather than being read.
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

// stoppedForConfigEdit gates both write paths. BepInEx reads a plugin's settings at load and
// may write them back at shutdown, so editing a running server's config is a change the
// server can overwrite without either side noticing (ADR-012, 12 §3.2).
func stoppedForConfigEdit(w http.ResponseWriter, r *http.Request, inst *store.Instance) bool {
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return false
	}
	return true
}

// saveConfig is the one write both paths go through: back the current bytes up, replace the
// file atomically, mark the instance as needing a restart, and audit it. A typed patch that
// skipped the backup would be the same data loss as a raw save that did, with a nicer form.
func (h *Instances) saveConfig(
	w http.ResponseWriter, r *http.Request, u *store.User, inst *store.Instance,
	path string, current, next []byte,
) bool {
	if err := fsutil.WriteFileAtomic(path+backupSuffix, current); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return false
	}
	// Written once and then left alone, so it keeps the file as it was before the panel
	// first touched it rather than as it was one save ago. Not a second backup: the .bak
	// undoes this write, and after five edits it is the only thing that still holds the
	// other four.
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

// configValidation maps the config package's field codes onto 11 §2.4's closed registry.
// The mapping exists because internal/mods/config imports neither api nor store, so it
// names its violations in its own terms.
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
