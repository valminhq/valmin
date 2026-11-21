package manager

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/instance"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// trackConfigs records, in each file's history, the config files a job is about to replace or
// remove, read from the copies the job's backup directory holds. names are relative to the
// config directory. before is the hash of each recorded file by name, and modWrote the hash of
// the version a mod install last wrote, for files whose history has one.
func trackConfigs(
	ctx context.Context, db *store.DB, inst *store.Instance, names []string, backupDir string,
) (before, modWrote map[string]string, err error) {
	before, modWrote = map[string]string{}, map[string]string{}
	var dir *os.Root
	defer func() {
		if dir != nil {
			_ = dir.Close()
		}
	}()
	for _, name := range names {
		saved := filepath.Join(backupDir, filepath.FromSlash(path.Join(instance.ConfigDir, name)))
		raw, err := os.ReadFile(saved)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read the saved %s: %w", name, err)
		}
		if dir == nil {
			if dir, err = instance.OpenConfigDir(inst.DataDir); err != nil {
				return nil, nil, fmt.Errorf("open config directory: %w", err)
			}
		}
		revs, err := db.ConfigRevisions(ctx, inst.ID, name)
		if err != nil {
			return nil, nil, err
		}
		if i := slices.IndexFunc(
			revs,
			func(r store.ConfigRevision) bool { return r.Source == store.ConfigMod },
		); i >= 0 {
			modWrote[name] = revs[i].SHA256
		}
		err = instance.TrackConfigBefore(ctx, db, dir, inst.ID, name, raw)
		if errors.Is(err, store.ErrConfigTooLarge) {
			slog.WarnContext(ctx, "config file too large to keep in its history; replaced without it",
				slog.String("instance_id", inst.ID), slog.String("file", name))
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(raw)
		before[name] = hex.EncodeToString(sum[:])
	}
	return before, modWrote, nil
}

// replacedConfigs is the config files an install overwrites, relative to the config directory.
func replacedConfigs(pkgs []*stagedPackage) []string {
	var out []string
	for _, p := range pkgs {
		for _, c := range p.changes {
			if name, ok := configName(c.Dest); ok && c.Action == installer.ActionOverwrite {
				out = append(out, name)
			}
		}
	}
	return out
}

// installedConfigRevisions reads the bytes an install wrote to config files as revisions, with
// the console fix-up applied to BepInEx.cfg as the install will apply it. A file over
// store.MaxConfigRevisionBytes is left out. before holds the hash each overwritten file had,
// and modWrote the hash a mod last wrote; a file matching neither that nor the previous
// version's shipped hash held edits.
func installedConfigRevisions(
	instanceID string, pkgs []*stagedPackage, before, modWrote map[string]string,
) ([]store.ConfigRevision, error) {
	var out []store.ConfigRevision
	for _, p := range pkgs {
		shipped := shippedHashes(p.prevManifest)
		from := ""
		if p.prev != nil {
			from = p.prev.Version
		}
		for _, c := range p.changes {
			name, ok := configName(c.Dest)
			if !ok || c.Action == installer.ActionSkip || !instance.KeepsHistory(name) {
				continue
			}
			raw, err := os.ReadFile(c.Source)
			if err != nil {
				return nil, fmt.Errorf("read the staged %s: %w", name, err)
			}
			if c.Dest == bepinexConfig {
				raw = modconfig.WithConsoleLogging(raw)
			}
			if len(raw) > store.MaxConfigRevisionBytes {
				continue
			}
			out = append(out, store.ConfigRevision{
				InstanceID: instanceID, File: name, Source: store.ConfigMod, Content: raw,
				Mod: p.fullName, FromVersion: from, ToVersion: p.version,
				ReplacedEdits: heldEdits(c.Dest, before[name], shipped[c.Dest], modWrote[name]),
			})
		}
	}
	return out, nil
}

// heldEdits reports whether a config file an install overwrote held bytes no mod wrote: had is
// its hash, empty when it was not recorded, shipped the hash the previous version shipped and
// modWrote the hash a mod last wrote, empty when none is recorded. BepInEx.cfg without one is
// not judged, since the panel's own console fix-up changes it after every framework install.
func heldEdits(dest, had, shipped, modWrote string) bool {
	if had == "" || had == shipped || had == modWrote {
		return false
	}
	return dest != bepinexConfig || modWrote != ""
}

// recordInstalledConfigs records revs inside the install's finish transaction, so the files
// and their history commit together. It is nil when there is nothing to record.
func recordInstalledConfigs(revs []store.ConfigRevision) func(context.Context, *sql.Tx) error {
	if len(revs) == 0 {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		return store.TxRecordConfigRevisions(ctx, tx, revs)
	}
}

// configName is dest relative to the config directory, and whether dest lies in it.
func configName(dest string) (string, bool) {
	return strings.CutPrefix(dest, instance.ConfigDir+"/")
}
