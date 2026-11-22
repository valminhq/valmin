package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"sort"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// SetupState captures and hashes the instance state stored in a saved setup.
type SetupState struct{ DB *store.DB }

// Current captures inst and returns the snapshot with its ETag, a hash over the snapshot, its
// config files and every managed file it names.
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
	for _, c := range snap.ConfigFiles {
		_, _ = io.WriteString(hash, c.File+"\x00"+c.SHA256+"\x00")
	}
	if err := hashSetupFiles(hash, inst, snap.Mods); err != nil {
		return snap, "", err
	}
	return snap, fmt.Sprintf("%q", hex.EncodeToString(hash.Sum(nil))), nil
}

// LaunchOf reads the launch half of an instances row. Modifiers that will not decode are an
// error, as they are to the container that would launch with them.
func LaunchOf(inst *store.Instance) (ManifestLaunch, error) {
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
		if err := json.Unmarshal([]byte(*inst.Modifiers), &launch.Modifiers); err != nil {
			return ManifestLaunch{}, fmt.Errorf("decode world modifiers of instance %s: %w", inst.ID, err)
		}
	}
	return launch, nil
}

// Capture reads inst's current setup: launch settings, installed mods, and the hash of every
// config file in the config tree apart from the RCON file.
func (s *SetupState) Capture(
	ctx context.Context, inst *store.Instance,
) (SetupSnapshot, error) {
	rows, err := s.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return SetupSnapshot{}, fmt.Errorf("read installed mods: %w", err)
	}
	configs, truncated, err := captureSetupConfigs(inst)
	if err != nil {
		return SetupSnapshot{}, err
	}
	launch, err := LaunchOf(inst)
	if err != nil {
		return SetupSnapshot{}, err
	}
	snap := SetupSnapshot{
		Instance: launch, Mods: make([]SetupMod, 0, len(rows)), Configs: []ManifestConfig{},
		ConfigTree: true, ConfigFiles: configs, ConfigsTruncated: truncated,
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

// captureSetupConfigs hashes the files instance.ConfigTree lists in inst's config directory,
// apart from the RCON file, sorted by path. A file gone since the walk is left out, and a server
// that has never started has none.
func captureSetupConfigs(inst *store.Instance) (configs []store.SetupConfigRef, truncated bool, err error) {
	root, err := instance.OpenConfigDir(inst.DataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return []store.SetupConfigRef{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = root.Close() }()
	files, truncated, err := instance.ConfigTree(root)
	if err != nil {
		return nil, false, err
	}
	configs = make([]store.SetupConfigRef, 0, len(files))
	for _, f := range files {
		if f.Path == command.ConfigFile {
			continue
		}
		raw, _, err := fsutil.ReadRegularIn(root, f.Path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsutil.ErrNotRegular) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("read configuration %s: %w", f.Path, err)
		}
		sum := sha256.Sum256(raw)
		configs = append(configs, store.SetupConfigRef{
			File: f.Path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(raw)),
		})
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].File < configs[j].File })
	return configs, truncated, nil
}

// retainSetupConfigs stores the bytes of each captured config file of inst in the blob store,
// failing when a file no longer matches the hash it was captured with.
func retainSetupConfigs(
	ctx context.Context, inst *store.Instance, blobs *setupblob.Store, configs []store.SetupConfigRef,
) error {
	if len(configs) == 0 {
		return nil
	}
	root, err := instance.OpenConfigDir(inst.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, c := range configs {
		raw, _, err := fsutil.ReadRegularIn(root, c.File)
		if err != nil {
			return fmt.Errorf("read configuration %s: %w", c.File, err)
		}
		digest, err := blobs.PutBytes(ctx, raw)
		if err != nil {
			return fmt.Errorf("retain configuration %s: %w", c.File, err)
		}
		if digest != c.SHA256 {
			return fmt.Errorf("configuration %s changed while the setup was saved", c.File)
		}
	}
	return nil
}

// ConfigPaths is the config files s holds, by path under the config directory.
func (s *SetupSnapshot) ConfigPaths() []string {
	paths := make([]string, 0, len(s.Configs)+len(s.ConfigFiles))
	for _, c := range s.Configs {
		paths = append(paths, c.File)
	}
	for _, c := range s.ConfigFiles {
		paths = append(paths, c.File)
	}
	return paths
}

// ScopedTo returns s with only the config files a restore of target manages: all of them for a
// target holding the config tree, the top-level .cfg files for one saved before.
func (s *SetupSnapshot) ScopedTo(target *SetupSnapshot) SetupSnapshot {
	scoped := *s
	if !target.ConfigTree {
		scoped.ConfigFiles = slices.DeleteFunc(slices.Clone(s.ConfigFiles), func(c store.SetupConfigRef) bool {
			return !CarriedConfig(c.File)
		})
	}
	return scoped
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
