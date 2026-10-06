package api

import (
	"context"
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
	Build   string                   `json:"build"`
	Launch  control.ManifestLaunch   `json:"launch"`
	Mods    []setupModView           `json:"mods"`
	Configs []control.ManifestConfig `json:"configs"`
}

type setupBackupView struct {
	ID         string    `json:"id"`
	WorldName  string    `json:"world_name"`
	CreatedAt  time.Time `json:"created_at"`
	Consistent bool      `json:"consistent"`
	Available  bool      `json:"available"`
}

type setupDetailView struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	CreatedAt   time.Time                `json:"created_at"`
	GameBuildID string                   `json:"game_build_id"`
	WorldName   string                   `json:"world_name"`
	Instance    control.ManifestLaunch   `json:"instance"`
	Mods        []setupModView           `json:"mods"`
	Configs     []control.ManifestConfig `json:"configs"`
	WorldBackup *setupBackupView         `json:"world_backup,omitempty"`
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

func (h *Instances) setupDetail(
	ctx context.Context, row *store.SavedSetup, snap *control.SetupSnapshot,
) (setupDetailView, error) {
	view := setupDetailView{
		ID: row.ID, Name: row.Name, CreatedAt: row.CreatedAt,
		GameBuildID: row.GameBuildID, WorldName: row.WorldName,
		Instance: snap.Instance, Mods: setupModsView(snap.Mods), Configs: snap.Configs,
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
		Consistent: b.Consistent, Available: available,
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
		return setupPreviewView{}, err //nolint:wrapcheck // preserve the setup error text
	}
	detail, err := h.setupDetail(ctx, row, &snap)
	if err != nil {
		return setupPreviewView{}, err
	}
	current, etag, err := h.currentSetupState(ctx, inst)
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
	targetPaths, err := control.SetupPaths(snap.Mods, snap.Configs)
	if err != nil {
		p.Problems = append(p.Problems, fmt.Sprintf("Saved setup files or configuration are invalid: %v", err))
	} else {
		currentSnap, err := h.ctl.SetupState.Capture(ctx, inst)
		if err != nil {
			return p, err //nolint:wrapcheck // preserve the setup error text
		}
		currentPaths, err := control.SetupPaths(currentSnap.Mods, currentSnap.Configs)
		if err != nil {
			return p, err //nolint:wrapcheck // preserve the setup error text
		}
		if err := control.PreflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
			p.Problems = append(p.Problems, fmt.Sprintf("Restore target conflicts with current files: %v", err))
		}
	}
	if err := control.ValidateSetupRefs(snap.Mods, refs); err != nil {
		p.Problems = append(p.Problems, "A saved package payload is missing or invalid.")
	}
	blobs := setupblob.New(h.Cfg.Data.Root)
	for _, ref := range refs {
		if _, err := blobs.Verify(ref.SHA256); err != nil {
			p.Problems = append(p.Problems, fmt.Sprintf("Package files for %s are unavailable.", ref.FullName))
		}
	}
	p.Ready = len(p.Problems) == 0
	return p, nil
}

func (h *Instances) currentSetupState(
	ctx context.Context, inst *store.Instance,
) (setupStateView, string, error) {
	snap, etag, err := h.ctl.SetupState.Current(ctx, inst)
	if err != nil {
		return setupStateView{}, "", err //nolint:wrapcheck // preserve the setup error text
	}
	return setupStateView{
		Build: deref(inst.GameBuildID), Launch: snap.Instance,
		Mods: setupModsView(snap.Mods), Configs: snap.Configs,
	}, etag, nil
}
