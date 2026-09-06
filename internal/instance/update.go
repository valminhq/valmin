package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
)

// serverDirName is the disposable game tree a update replaces wholesale (02 §3, 08 §4.1).
const serverDirName = "server"

// ServerDir is the instance's game installation.
func ServerDir(dataDir string) string { return filepath.Join(dataDir, serverDirName) }

// StagedServerDir is the replacement tree an update builds beside the live one. Its suffix is
// the one a restore stages a world under, because the swap that publishes it is the same.
func StagedServerDir(dataDir string) string { return ServerDir(dataDir) + backup.StagedSuffix }

// UpdateStaging holds what an update gathers before it publishes anything: the build it is
// installing, and the files that have to survive the new clone. Outside server/, because
// server/ is what gets replaced (ADR-138).
func UpdateStaging(dataDir string) string { return filepath.Join(dataDir, ".game-update") }

// UpdateReplayDir is where the caller gathers package files and user configs, in the order it
// wants them applied.
func UpdateReplayDir(dataDir string) string { return filepath.Join(UpdateStaging(dataDir), "files") }

// buildIDFile records which build the staged tree is, so recovery can say what it is looking
// at rather than infer it.
const buildIDFile = "build-id"

// HasDoorstop reports whether the instance is modded, by the same filesystem test the
// container entrypoint uses (ADR-107). The installed-mods table answers a different question:
// a framework placed by hand is still one an update will replace.
func HasDoorstop(dataDir string) (bool, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return false, fmt.Errorf("open instance: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Stat("server/doorstop_libs/libdoorstop_x64.so")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check Doorstop: %w", err)
	}
	return info.Mode().IsRegular(), nil
}

// StageUpdate clones the cached build into server.new, beside the live tree and on the same
// filesystem so publishing it is a rename. It leaves server/ untouched.
//
// StagedServerDir is what the caller then verifies the ownership of, the same seam
// provisioning uses: the clone happens here, A4's uid assertion belongs to the job.
func StageUpdate(ctx context.Context, dataDir, cacheRoot, buildID string) error {
	if _, err := validSteamBuild(buildID); err != nil {
		return err
	}
	cache := filepath.Join(cacheRoot, buildID)
	if _, err := cachedBuildID(cache, buildID); err != nil {
		return err
	}

	staged := StagedServerDir(dataDir)
	if err := os.RemoveAll(staged); err != nil {
		return fmt.Errorf("clear the staged server: %w", err)
	}
	if err := CloneWithProgress(ctx, cache, staged, time.Second, func(int) {}); err != nil {
		return err
	}

	if err := fsutil.MkdirAllExact(UpdateStaging(dataDir)); err != nil {
		return fmt.Errorf("create update staging: %w", err)
	}
	marker := filepath.Join(UpdateStaging(dataDir), buildIDFile)
	if err := fsutil.WriteFileAtomic(marker, []byte(buildID)); err != nil {
		return fmt.Errorf("record the staged build: %w", err)
	}
	return nil
}

// SaveUpdateConfigs copies the user's whole config tree into the replay directory, `.bak` and
// `.orig` included: those are edits and saved copies a package manifest cannot reconstruct
// (ADR-125, ADR-138). An instance with no config directory has nothing to save.
func SaveUpdateConfigs(dataDir string) error {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return fmt.Errorf("open instance: %w", err)
	}
	defer func() { _ = root.Close() }()

	config, err := root.OpenRoot("server/BepInEx/config")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open user configs: %w", err)
	}
	defer func() { _ = config.Close() }()

	if err := installer.CopyTree(config, filepath.Join(UpdateReplayDir(dataDir), "BepInEx", "config")); err != nil {
		return fmt.Errorf("save the user's configs: %w", err)
	}
	return nil
}

// ApplyStagedFiles overlays the replay directory onto the staged server, so what the caller
// gathered last wins over the build's shipped defaults.
func ApplyStagedFiles(dataDir string) error {
	files, err := os.OpenRoot(UpdateReplayDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		// A vanilla instance with no configs of its own: the clone is already what it needs.
		return nil
	}
	if err != nil {
		return fmt.Errorf("open the staged files: %w", err)
	}
	defer func() { _ = files.Close() }()
	if err := installer.CopyTree(files, StagedServerDir(dataDir)); err != nil {
		return fmt.Errorf("overlay the staged files: %w", err)
	}
	return nil
}

// SwapUpdate publishes the staged server through the same two renames a restore publishes a
// world with: server/ aside, staged into place, aside removed. One implementation, so the
// panel has one answer to being killed between the renames (12 §9.4).
func SwapUpdate(dataDir string) error {
	if err := backup.Swap(ServerDir(dataDir)); err != nil {
		return fmt.Errorf("replace the server: %w", err)
	}
	return nil
}

// RecoverUpdate resolves whatever an interrupted update left on disk and reports what it did.
// Every outcome leaves exactly one server/ tree.
//
// It does not re-clone. The staged tree is complete before the first rename — the clone, the
// manifest replay and the configs all land ahead of the `swap_started` checkpoint — so a
// staged tree that exists while server/ does not is one that was proven ready, and finishing
// the rename is both faster and more certain than fetching a build the public branch may since
// have moved past.
func RecoverUpdate(dataDir string) (string, error) {
	action, err := backup.RecoverSwap(ServerDir(dataDir))
	if err != nil {
		return "", fmt.Errorf("resolve an interrupted update: %w", err)
	}
	if err := os.RemoveAll(UpdateStaging(dataDir)); err != nil {
		return "", fmt.Errorf("clear the update staging: %w", err)
	}
	return action, nil
}

// StagedBuildID reads which build an interrupted update was installing, for the log line that
// tells an operator what they are looking at. A missing or unreadable marker is "".
func StagedBuildID(dataDir string) string {
	raw, err := os.ReadFile(filepath.Join(UpdateStaging(dataDir), buildIDFile))
	if err != nil {
		return ""
	}
	id, err := validSteamBuild(strings.TrimSpace(string(raw)))
	if err != nil {
		return ""
	}
	return id
}
