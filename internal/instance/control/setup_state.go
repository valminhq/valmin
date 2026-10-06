package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

const (
	setupConfigDir     = "BepInEx/config"
	maxSetupConfigSize = 1 << 20
)

// SetupState captures and hashes the instance state stored in a saved setup.
type SetupState struct{ DB *store.DB }

func (s *SetupState) Current(ctx context.Context, inst *store.Instance) (SetupSnapshot, string, error) {
	snap, err := s.Capture(ctx, inst)
	if err != nil {
		return SetupSnapshot{}, "", err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return snap, "", fmt.Errorf("encode current setup: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write(raw)
	_, _ = io.WriteString(hash, deref(inst.GameBuildID))
	if err := hashSetupFiles(hash, inst, snap.Mods); err != nil {
		return snap, "", err
	}
	return snap, fmt.Sprintf("%q", hex.EncodeToString(hash.Sum(nil))), nil
}

func LaunchOf(inst *store.Instance) ManifestLaunch {
	launch := ManifestLaunch{
		ServerName: inst.ServerName, WorldName: inst.WorldName,
		Public: inst.Public, Crossplay: inst.Crossplay,
		MemLimitMB: inst.MemLimitMB, CPULimit: inst.CPULimit,
		BackupKeepCold: inst.BackupKeepCold, BackupKeepHot: inst.BackupKeepHot,
		BackupOnRestart: inst.BackupOnRestart,
	}
	if inst.Preset != nil {
		launch.Preset = *inst.Preset
	}
	if inst.ExtraArgs != nil {
		launch.ExtraArgs = *inst.ExtraArgs
	}
	if inst.Modifiers != nil && *inst.Modifiers != "" {
		_ = json.Unmarshal([]byte(*inst.Modifiers), &launch.Modifiers)
	}
	return launch
}

func (s *SetupState) Capture(
	ctx context.Context, inst *store.Instance,
) (SetupSnapshot, error) {
	rows, err := s.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return SetupSnapshot{}, fmt.Errorf("read installed mods: %w", err)
	}
	configs, err := readSetupConfigs(inst)
	if err != nil {
		return SetupSnapshot{}, err
	}
	snap := SetupSnapshot{
		Instance: LaunchOf(inst), Mods: make([]SetupMod, 0, len(rows)), Configs: configs,
	}
	for i := range rows {
		m := &rows[i]
		snap.Mods = append(snap.Mods, SetupMod{
			FullName: m.FullName, Source: m.Source.String(), Version: m.Version,
			InstalledAs: m.InstalledAs, Side: m.Side, Enabled: m.Enabled,
			Locked: m.Locked, FileManifest: m.FileManifest,
		})
	}
	return snap, nil
}

func readSetupConfigs(inst *store.Instance) ([]ManifestConfig, error) {
	dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(setupConfigDir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []ManifestConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open configuration directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	out := []ManifestConfig{}
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
		raw, readErr := io.ReadAll(io.LimitReader(f, maxSetupConfigSize+1))
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

func setupConfigText(name string, raw []byte) (ManifestConfig, error) {
	if len(raw) > maxSetupConfigSize {
		return ManifestConfig{}, fmt.Errorf("configuration %s exceeds the supported size", name)
	}
	if !utf8.Valid(raw) {
		return ManifestConfig{}, fmt.Errorf("configuration %s is not valid UTF-8 text", name)
	}
	return ManifestConfig{File: name, Content: string(raw)}, nil
}

func hashSetupFiles(hash io.Writer, inst *store.Instance, mods []SetupMod) error {
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
			f, err := OpenManagedSetupFile(inst, mod.FullName, entry)
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
