package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

type setupModView struct {
	FullName    string `json:"full_name"`
	Source      string `json:"source"`
	Version     string `json:"version"`
	InstalledAs string `json:"installed_as"`
	Side        string `json:"side"`
	Enabled     bool   `json:"enabled"`
	Locked      bool   `json:"locked"`
}

type setupStateView struct {
	Build   string                 `json:"build"`
	Launch  control.ManifestLaunch `json:"launch"`
	Mods    []setupModView         `json:"mods"`
	Configs []setupConfigView      `json:"configs"`
}

// setupConfigView is one config file of a setup or of the current server, by path under
// BepInEx/config. Content is set for a top-level .cfg file only; the SPA reads any other file
// through the saved-file and config routes.
type setupConfigView struct {
	File      string  `json:"file"`
	Content   *string `json:"content,omitempty"`
	SHA256    string  `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
}

type setupBackupView struct {
	ID         string    `json:"id"`
	WorldName  string    `json:"world_name"`
	CreatedAt  time.Time `json:"created_at"`
	Consistent bool      `json:"consistent"`
	Available  bool      `json:"available"`
	SizeBytes  int64     `json:"size_bytes"`
}

type setupDetailView struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	CreatedAt   time.Time              `json:"created_at"`
	GameBuildID string                 `json:"game_build_id"`
	WorldName   string                 `json:"world_name"`
	Instance    control.ManifestLaunch `json:"instance"`
	Mods        []setupModView         `json:"mods"`
	Configs     []setupConfigView      `json:"configs"`
	WorldBackup *setupBackupView       `json:"world_backup,omitempty"`
	// ConfigTree is false for a setup saved before setups held the whole config tree, which
	// restores only top-level .cfg files.
	ConfigTree bool `json:"config_tree"`
	// ConfigsTruncated reports that the config tree held files past the listing limits, which
	// the setup left out.
	ConfigsTruncated bool `json:"configs_truncated"`
	// Automatic marks a setup the panel saved before a restore or a mod update.
	Automatic bool `json:"automatic"`
	// Light marks a setup that keeps only the packages their registry archives cannot
	// reproduce, and downloads the rest on restore.
	Light bool `json:"light"`
}

type setupPreviewView struct {
	ETag     string          `json:"etag"`
	Ready    bool            `json:"ready"`
	Problems []string        `json:"problems"`
	Current  setupStateView  `json:"current"`
	Setup    setupDetailView `json:"setup"`
}

func setupModsView(mods []control.SetupMod) []setupModView {
	out := make([]setupModView, 0, len(mods))
	for _, m := range mods {
		out = append(out, setupModView{
			FullName: m.FullName, Source: m.Source, Version: m.Version,
			InstalledAs: m.InstalledAs, Side: m.Side, Enabled: m.Enabled, Locked: m.Locked,
		})
	}
	return out
}

func (h *Instances) setupRoutes(rt *routeTable) {
	rt.Handle("GET /api/v1/instances/{id}/setups", http.HandlerFunc(h.listSetups))
	rt.Handle("POST /api/v1/instances/{id}/setups", http.HandlerFunc(h.saveSetup))
	rt.Handle("GET /api/v1/instances/{id}/setups/{sid}", http.HandlerFunc(h.getSetup))
	rt.Handle("GET /api/v1/instances/{id}/setups/{sid}/configs/{file}", http.HandlerFunc(h.readSetupConfig))
	rt.Handle("GET /api/v1/instances/{id}/setups/{sid}/preview", http.HandlerFunc(h.previewSetup))
	rt.Handle("POST /api/v1/instances/{id}/setups/{sid}/restore", http.HandlerFunc(h.restoreSetup))
	rt.Handle("DELETE /api/v1/instances/{id}/setups/{sid}", http.HandlerFunc(h.deleteSetup))
}

func (h *Instances) setupInstance(w http.ResponseWriter, r *http.Request) (*store.Instance, bool) {
	return h.mustLoadInstance(w, r, r.PathValue("id"))
}

func (h *Instances) listSetups(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	rows, err := h.DB.ListSetups(r.Context(), inst.ID)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	items := make([]setupDetailView, 0, len(rows))
	for i := range rows {
		snap, err := control.DecodeSetupSnapshot(&rows[i])
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		item, err := h.setupDetail(r.Context(), &rows[i], &snap)
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		item.Configs = nil
		items = append(items, item)
	}
	JSON(w, r, http.StatusOK, map[string]any{"items": items})
}

func (h *Instances) getSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, _, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	snap, err := control.DecodeSetupSnapshot(row)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	view, err := h.setupDetail(r.Context(), row, &snap)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, view)
}

// readSetupConfig handles GET /instances/{id}/setups/{sid}/configs/{file}: the saved bytes of
// one config file of a setup, as text. file is a path under BepInEx/config with its slashes
// escaped; a file the setup does not hold is a 404.
func (h *Instances) readSetupConfig(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, _, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	snap, err := control.DecodeSetupSnapshot(row)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	raw, found, err := savedConfig(setupblob.New(h.Cfg.Data.Root), &snap, r.PathValue("file"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if !found {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // served as text/plain, never interpreted
	_, _ = w.Write(raw)
}

// savedConfig returns the saved bytes of the config file at name in snap, reporting whether
// snap holds it.
func savedConfig(
	blobs *setupblob.Store, snap *control.SetupSnapshot, name string,
) (raw []byte, found bool, err error) {
	for _, c := range snap.Configs {
		if c.File == name {
			return []byte(c.Content), true, nil
		}
	}
	for _, c := range snap.ConfigFiles {
		if c.File == name {
			raw, err = control.ReadSetupConfig(blobs, c)
			return raw, err == nil, err
		}
	}
	return nil, false, nil
}

func (h *Instances) setupDetail(
	ctx context.Context, row *store.SavedSetup, snap *control.SetupSnapshot,
) (setupDetailView, error) {
	view := setupDetailView{
		ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt,
		GameBuildID: row.GameBuildID, WorldName: row.WorldName,
		Instance: snap.Instance, Mods: setupModsView(snap.Mods),
		Configs:    savedConfigViews(setupblob.New(h.Cfg.Data.Root), snap),
		ConfigTree: snap.ConfigTree, ConfigsTruncated: snap.ConfigsTruncated, Automatic: row.Automatic,
		Light: snap.Light,
	}
	if row.BackupID == "" {
		return view, nil
	}
	b, err := h.DB.BackupByID(ctx, row.InstanceID, row.BackupID)
	if err != nil {
		return view, fmt.Errorf("read linked backup %s: %w", row.BackupID, err)
	}
	if b == nil {
		view.WorldBackup = &setupBackupView{ID: row.BackupID}
		return view, nil
	}
	available, err := setupBackupAvailable(h.Cfg.Data.Root, b.Path)
	view.WorldBackup = &setupBackupView{
		ID: b.ID, WorldName: b.WorldName, CreatedAt: b.CreatedAt,
		Consistent: b.Consistent, Available: available, SizeBytes: b.SizeBytes,
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return view, fmt.Errorf("inspect linked backup %s: %w", b.ID, err)
	}
	return view, nil
}

func setupBackupAvailable(dataRoot, path string) (bool, error) {
	rootPath := instance.BackupsDir(dataRoot)
	rel, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(rel) {
		return false, nil
	}
	root, err := os.OpenRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open backup directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	_, err = root.Stat(rel)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect backup archive: %w", err)
	}
	return true, nil
}

func (h *Instances) previewSetup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, refs, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	preview, err := h.setupPreview(r.Context(), inst, row, refs)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	w.Header().Set("ETag", preview.ETag)
	JSON(w, r, http.StatusOK, preview)
}

func (h *Instances) setupPreview(
	ctx context.Context, inst *store.Instance, row *store.SavedSetup, refs []store.SetupArtifactRef,
) (setupPreviewView, error) {
	snap, err := control.DecodeSetupSnapshot(row)
	if err != nil {
		return setupPreviewView{}, err
	}
	detail, err := h.setupDetail(ctx, row, &snap)
	if err != nil {
		return setupPreviewView{}, err
	}
	current, currentSnap, etag, err := h.currentSetupState(ctx, inst, &snap)
	if err != nil {
		return setupPreviewView{}, err
	}
	p := setupPreviewView{
		ETag: etag, Problems: []string{},
		Current: current, Setup: detail,
	}
	if inst.State != string(instance.StateStopped) {
		p.Problems = append(p.Problems, "Stop the server before restoring this setup.")
	}
	if err := h.assertStopped(ctx, inst); err != nil {
		if errors.Is(err, instance.ErrServerRunning) {
			p.Problems = append(p.Problems, "The game container is running. Stop it before restoring.")
		} else {
			return p, err
		}
	}
	if err := h.ctl.SetupJobs.ValidateSettings(ctx, inst, &snap.Instance); err != nil {
		p.Problems = append(p.Problems, err.Error())
	}
	targetPaths, err := control.SetupPaths(snap.Mods, snap.ConfigPaths())
	if err != nil {
		p.Problems = append(p.Problems, fmt.Sprintf("Saved setup files or configuration are invalid: %v", err))
	} else {
		currentPaths, err := control.SetupPaths(currentSnap.Mods, currentSnap.ConfigPaths())
		if err != nil {
			return p, err
		}
		if err := control.PreflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
			p.Problems = append(p.Problems, fmt.Sprintf("Restore target conflicts with current files: %v", err))
		}
	}
	if err := control.ValidateSetupRefs(&snap, refs); err != nil {
		p.Problems = append(p.Problems, "A saved package payload is missing or invalid.")
	}
	missing, err := h.ctl.SetupJobs.UnavailableArchives(ctx, &snap, refs)
	if err != nil {
		return p, err
	}
	for _, name := range missing {
		p.Problems = append(p.Problems, fmt.Sprintf("%s is not in the download cache and no enabled "+
			"registry lists it, so this light setup cannot restore it.", name))
	}
	p.Problems = append(p.Problems, unavailableSetupBlobs(setupblob.New(h.Cfg.Data.Root), refs, &snap)...)
	p.Ready = len(p.Problems) == 0
	return p, nil
}

// unavailableSetupBlobs names each saved package and config file of a setup whose blob is
// missing or no longer matches its digest.
func unavailableSetupBlobs(
	blobs *setupblob.Store, refs []store.SetupArtifactRef, snap *control.SetupSnapshot,
) []string {
	var problems []string
	for _, ref := range refs {
		if _, err := blobs.Verify(ref.SHA256); err != nil {
			problems = append(problems, fmt.Sprintf("Package files for %s are unavailable.", ref.FullName))
		}
	}
	for _, c := range snap.ConfigFiles {
		if _, err := blobs.Verify(c.SHA256); err != nil {
			problems = append(problems, fmt.Sprintf("The saved %s is unavailable.", c.File))
		}
	}
	return problems
}

// currentSetupState captures inst's setup as a restore of target would compare it: with only
// the config files that restore manages. It returns the view, the full capture cut to that
// scope, and the capture's ETag.
func (h *Instances) currentSetupState(
	ctx context.Context, inst *store.Instance, target *control.SetupSnapshot,
) (setupStateView, control.SetupSnapshot, string, error) {
	snap, etag, err := h.ctl.SetupState.Current(ctx, inst)
	if err != nil {
		return setupStateView{}, control.SetupSnapshot{}, "", err
	}
	snap = snap.ScopedTo(target)
	configs, err := currentConfigViews(inst, snap.ConfigFiles)
	if err != nil {
		return setupStateView{}, control.SetupSnapshot{}, "", err
	}
	return setupStateView{
		Build: deref(inst.GameBuildID), Launch: snap.Instance,
		Mods: setupModsView(snap.Mods), Configs: configs,
	}, snap, etag, nil
}

// savedConfigViews lists the config files of a saved setup, with the content of each top-level
// .cfg file. A saved file whose blob cannot be read is listed without content; the preview
// reports it.
func savedConfigViews(blobs *setupblob.Store, snap *control.SetupSnapshot) []setupConfigView {
	views := make([]setupConfigView, 0, len(snap.Configs)+len(snap.ConfigFiles))
	for _, c := range snap.Configs {
		sum := sha256.Sum256([]byte(c.Content))
		content := c.Content
		views = append(views, setupConfigView{
			File: c.File, Content: &content,
			SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(c.Content)),
		})
	}
	for _, c := range snap.ConfigFiles {
		v := setupConfigView{File: c.File, SHA256: c.SHA256, SizeBytes: c.SizeBytes}
		if control.CarriedConfig(c.File) {
			if raw, err := control.ReadSetupConfig(blobs, c); err == nil {
				content := string(raw)
				v.Content = &content
			}
		}
		views = append(views, v)
	}
	return views
}

// currentConfigViews lists the server's captured config files, with the content of each
// top-level .cfg file read from disk.
func currentConfigViews(inst *store.Instance, configs []store.SetupConfigRef) ([]setupConfigView, error) {
	views := make([]setupConfigView, 0, len(configs))
	if len(configs) == 0 {
		return views, nil
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	for _, c := range configs {
		v := setupConfigView{File: c.File, SHA256: c.SHA256, SizeBytes: c.SizeBytes}
		if control.CarriedConfig(c.File) {
			raw, _, err := fsutil.ReadRegularIn(dir, c.File)
			if err != nil {
				return nil, fmt.Errorf("read configuration %s: %w", c.File, err)
			}
			content := string(raw)
			v.Content = &content
		}
		views = append(views, v)
	}
	return views, nil
}
