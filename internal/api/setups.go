package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

type setupMod struct {
	FullName     string `json:"full_name"`
	Source       string `json:"source"`
	Version      string `json:"version"`
	InstalledAs  string `json:"installed_as"`
	Side         string `json:"side"`
	Enabled      bool   `json:"enabled"`
	Locked       bool   `json:"locked"`
	FileManifest string `json:"file_manifest"`
}

type setupSnapshot struct {
	Instance manifestLaunch   `json:"instance"`
	Mods     []setupMod       `json:"mods"`
	Configs  []manifestConfig `json:"configs"`
}

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
	Build   string           `json:"build"`
	Launch  manifestLaunch   `json:"launch"`
	Mods    []setupModView   `json:"mods"`
	Configs []manifestConfig `json:"configs"`
}

type setupBackupView struct {
	ID         string    `json:"id"`
	WorldName  string    `json:"world_name"`
	CreatedAt  time.Time `json:"created_at"`
	Consistent bool      `json:"consistent"`
	Available  bool      `json:"available"`
}

type setupDetailView struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	CreatedAt   time.Time        `json:"created_at"`
	GameBuildID string           `json:"game_build_id"`
	WorldName   string           `json:"world_name"`
	Instance    manifestLaunch   `json:"instance"`
	Mods        []setupModView   `json:"mods"`
	Configs     []manifestConfig `json:"configs"`
	WorldBackup *setupBackupView `json:"world_backup,omitempty"`
}

type setupPreviewView struct {
	ETag     string          `json:"etag"`
	Ready    bool            `json:"ready"`
	Problems []string        `json:"problems"`
	Current  setupStateView  `json:"current"`
	Setup    setupDetailView `json:"setup"`
}

func setupModsView(mods []setupMod) []setupModView {
	out := make([]setupModView, 0, len(mods))
	for _, m := range mods {
		out = append(out, setupModView{
			FullName: m.FullName, Source: m.Source, Version: m.Version,
			InstalledAs: m.InstalledAs, Side: m.Side, Enabled: m.Enabled, Locked: m.Locked,
		})
	}
	return out
}

func (h *Instances) setupRoutes(rt *Router) {
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	rows, err := h.DB.ListSetups(r.Context(), inst.ID)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	items := make([]setupDetailView, 0, len(rows))
	for i := range rows {
		snap, err := decodeSetupSnapshot(&rows[i])
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
			return
		}
		item, err := h.setupDetail(r.Context(), &rows[i], &snap)
		if err != nil {
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, _, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	snap, err := decodeSetupSnapshot(row)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	view, err := h.setupDetail(r.Context(), row, &snap)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, view)
}

func decodeSetupSnapshot(row *store.SavedSetup) (setupSnapshot, error) {
	var snap setupSnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snap); err != nil {
		return snap, fmt.Errorf("decode saved setup %s: %w", row.ID, err)
	}
	return snap, nil
}

func (h *Instances) setupDetail(
	ctx context.Context, row *store.SavedSetup, snap *setupSnapshot,
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
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SetupsManage, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.setupInstance(w, r)
	if !ok {
		return
	}
	row, refs, err := h.DB.SetupByID(r.Context(), inst.ID, r.PathValue("sid"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	preview, err := h.setupPreview(r.Context(), inst, row, refs)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.Header().Set("ETag", preview.ETag)
	JSON(w, r, http.StatusOK, preview)
}

func (h *Instances) setupPreview(
	ctx context.Context, inst *store.Instance, row *store.SavedSetup, refs []store.SetupArtifactRef,
) (setupPreviewView, error) {
	snap, err := decodeSetupSnapshot(row)
	if err != nil {
		return setupPreviewView{}, err
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
		if errors.Is(err, errServerRunning) {
			p.Problems = append(p.Problems, "The game container is running. Stop it before restoring.")
		} else {
			return p, err
		}
	}
	if err := h.validateSetupSettings(ctx, inst, &snap.Instance); err != nil {
		p.Problems = append(p.Problems, err.Error())
	}
	targetPaths, err := setupPaths(snap.Mods, snap.Configs)
	if err != nil {
		p.Problems = append(p.Problems, fmt.Sprintf("Saved setup files or configuration are invalid: %v", err))
	} else {
		currentSnap, err := h.captureSetupSnapshot(ctx, inst)
		if err != nil {
			return p, err
		}
		currentPaths, err := setupPaths(currentSnap.Mods, currentSnap.Configs)
		if err != nil {
			return p, err
		}
		if err := preflightSetupTargets(inst, currentPaths, targetPaths); err != nil {
			p.Problems = append(p.Problems, fmt.Sprintf("Restore target conflicts with current files: %v", err))
		}
	}
	if err := validateSetupRefs(snap.Mods, refs); err != nil {
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

func (h *Instances) validateSetupSettings(
	ctx context.Context, inst *store.Instance, saved *manifestLaunch,
) error {
	password, err := h.decryptPassword(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read current server password: %w", err)
	}
	if len(instance.ValidateLaunch(saved.ServerName, inst.WorldName, password)) > 0 {
		return errors.New("saved server name conflicts with the current world name or password")
	}
	if len(instance.ValidateResources(saved.MemLimitMB, saved.CPULimit)) > 0 {
		return errors.New("saved resource limits are no longer valid")
	}
	if saved.BackupKeepCold < 0 || saved.BackupKeepHot < 0 {
		return errors.New("saved backup retention is invalid")
	}
	return nil
}

func (h *Instances) currentSetupState(
	ctx context.Context, inst *store.Instance,
) (setupStateView, string, error) {
	snap, err := h.captureSetupSnapshot(ctx, inst)
	if err != nil {
		return setupStateView{}, "", err
	}
	view := setupStateView{
		Build: deref(inst.GameBuildID), Launch: snap.Instance,
		Mods: setupModsView(snap.Mods), Configs: snap.Configs,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return view, "", fmt.Errorf("encode current setup: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write(raw)
	_, _ = io.WriteString(hash, deref(inst.GameBuildID))
	if err := hashSetupFiles(hash, inst, snap.Mods); err != nil {
		return view, "", err
	}
	return view, fmt.Sprintf("%q", hex.EncodeToString(hash.Sum(nil))), nil
}

func (h *Instances) captureSetupSnapshot(
	ctx context.Context, inst *store.Instance,
) (setupSnapshot, error) {
	rows, err := h.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return setupSnapshot{}, fmt.Errorf("read installed mods: %w", err)
	}
	configs, err := readSetupConfigs(inst)
	if err != nil {
		return setupSnapshot{}, err
	}
	snap := setupSnapshot{
		Instance: launchOf(inst), Mods: make([]setupMod, 0, len(rows)), Configs: configs,
	}
	for i := range rows {
		m := &rows[i]
		snap.Mods = append(snap.Mods, setupMod{
			FullName: m.FullName, Source: m.Source.String(), Version: m.Version,
			InstalledAs: m.InstalledAs, Side: m.Side, Enabled: m.Enabled,
			Locked: m.Locked, FileManifest: m.FileManifest,
		})
	}
	return snap, nil
}

func readSetupConfigs(inst *store.Instance) ([]manifestConfig, error) {
	dir := filepath.Join(serverDir(inst), filepath.FromSlash(configDir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []manifestConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open configuration directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	out := []manifestConfig{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".cfg") || name == command.ConfigFile {
			continue
		}
		f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open configuration %s: %w", name, err)
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			if statErr != nil {
				return nil, fmt.Errorf("inspect configuration %s: %w", name, statErr)
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, maxManifestConfigSize+1))
		_ = f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read configuration %s: %w", name, readErr)
		}
		cfg, err := setupConfigText(name, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

func setupConfigText(name string, raw []byte) (manifestConfig, error) {
	if len(raw) > maxManifestConfigSize {
		return manifestConfig{}, fmt.Errorf("configuration %s exceeds the supported size", name)
	}
	if !utf8.Valid(raw) {
		return manifestConfig{}, fmt.Errorf("configuration %s is not valid UTF-8 text", name)
	}
	return manifestConfig{File: name, Content: string(raw)}, nil
}

func hashSetupFiles(hash io.Writer, inst *store.Instance, mods []setupMod) error {
	for _, mod := range mods {
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(mod.FileManifest), &manifest); err != nil {
			return fmt.Errorf("decode %s file manifest: %w", mod.FullName, err)
		}
		for _, entry := range manifest {
			if installer.UserConfig(entry.Path) {
				continue
			}
			_, _ = io.WriteString(hash, mod.FullName+"\x00"+entry.Path+"\x00")
			f, err := openManagedSetupFile(inst, mod.FullName, entry)
			if errors.Is(err, os.ErrNotExist) {
				_, _ = io.WriteString(hash, "missing\x00")
				continue
			}
			if err != nil {
				return err
			}
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			_ = f.Close()
			if copyErr != nil {
				return fmt.Errorf("hash %s: %w", entry.Path, copyErr)
			}
			_, _ = hash.Write(h.Sum(nil))
		}
	}
	return nil
}
