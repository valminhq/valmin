package control

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// CheckManifestConfigName accepts one plain .cfg filename: no directory, no separator, no ".."
// and no control character.
func CheckManifestConfigName(name string) error {
	if name == "" {
		return fmt.Errorf("a config entry has no filename")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%q is not a plain filename", name)
	}
	if !strings.HasSuffix(name, ".cfg") {
		return fmt.Errorf("%q is not a .cfg file", name)
	}
	return nil
}

// CarriedConfig reports whether the config file at name, relative to the config directory, is a
// plain .cfg other than the RCON plugin's: what a setup saved before setups held the config tree
// carried, and what a schema 1 definition can hold.
func CarriedConfig(name string) bool {
	return CheckManifestConfigName(name) == nil && name != command.ConfigFile
}

// ApplyManifestConfigs writes the configuration files of an imported definition, each at its
// path under the config directory, creating the folders it needs. With merge set, a .cfg file's
// settings are applied over the file already on disk instead of replacing it; any other file is
// replaced whole. Each file's history records the bytes a write replaces and the bytes written;
// a file whose bytes would not change is not rewritten.
func ApplyManifestConfigs(
	ctx context.Context, db *store.DB, inst *store.Instance, configs []ManifestConfig, merge bool,
) error {
	if len(configs) == 0 {
		return nil
	}
	for _, cfg := range configs {
		if err := instance.CheckConfigPath(cfg.File); err != nil {
			return err
		}
	}
	if err := mkdirConfigDir(inst.DataDir); err != nil {
		return err
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for _, cfg := range configs {
		if err := applyManifestConfig(ctx, db, inst.ID, dir, cfg, merge); err != nil {
			return err
		}
	}
	return nil
}

// applyManifestConfig writes one imported file inside dir, recording the bytes it replaces and
// the bytes it writes. A file that did not exist is recorded empty, so the imported settings
// count as tweaks.
func applyManifestConfig(
	ctx context.Context, db *store.DB, instanceID string, dir *os.Root, cfg ManifestConfig, merge bool,
) error {
	current, _, err := fsutil.ReadRegularIn(dir, cfg.File)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read config %s: %w", cfg.File, err)
	}
	next := []byte(cfg.Content)
	if merge && strings.HasSuffix(cfg.File, ".cfg") {
		next = modconfig.Merge(current, next)
	}
	if err := modconfig.DropPending(dir, cfg.File); err != nil {
		return err
	}
	if exists && bytes.Equal(current, next) {
		return nil
	}
	if err := trackBefore(ctx, db, dir, instanceID, cfg.File, current); err != nil {
		return err
	}
	if err := mkdirAllIn(dir, path.Dir(cfg.File)); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomicIn(dir, cfg.File, next); err != nil {
		return fmt.Errorf("write config %s: %w", cfg.File, err)
	}
	recordConfigWrite(ctx, db, instanceID, cfg.File, next)
	return nil
}

// trackBefore is instance.TrackConfigBefore for a job's write, which goes ahead without the old
// version of a file too large to keep, logging that it was not kept.
func trackBefore(ctx context.Context, db *store.DB, dir *os.Root, instanceID, file string, current []byte) error {
	err := instance.TrackConfigBefore(ctx, db, dir, instanceID, file, current)
	if errors.Is(err, store.ErrConfigTooLarge) {
		slog.WarnContext(ctx, "config file too large to keep in its history; replaced without it",
			slog.String("instance_id", instanceID), slog.String("file", file))
		return nil
	}
	return err
}

// recordConfigWrite records bytes a setup restore or definition import wrote. A failure is only
// logged: the file is written, and the next write records it as an outside change.
func recordConfigWrite(ctx context.Context, db *store.DB, instanceID, file string, content []byte) {
	if !instance.KeepsHistory(file) {
		return
	}
	err := db.RecordConfigRevisions(ctx, []store.ConfigRevision{{
		InstanceID: instanceID, File: file, Source: store.ConfigSetup, Content: content,
	}})
	if err != nil {
		slog.WarnContext(ctx, "config revision not recorded",
			slog.String("instance_id", instanceID), slog.String("file", file), slog.Any("error", err))
	}
}

// mkdirConfigDir creates the config directory inside the server tree, with the exact mode
// fsutil.MkdirAllExact gives, without following a symlink out of the tree.
func mkdirConfigDir(dataDir string) error {
	server, err := os.OpenRoot(instance.ServerDir(dataDir))
	if err != nil {
		return fmt.Errorf("open server directory: %w", err)
	}
	defer func() { _ = server.Close() }()
	return mkdirAllIn(server, instance.ConfigDir)
}

// mkdirAllIn creates each missing folder of the slash path rel inside root, with the exact mode
// fsutil.MkdirAllExact gives. An existing folder, or a symlink root resolves inside itself, is
// kept; root refuses one that leads out.
func mkdirAllIn(root *os.Root, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	var p string
	for _, part := range strings.Split(rel, "/") {
		p = filepath.Join(p, part)
		if _, err := root.Lstat(p); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
		if err := root.Mkdir(p, fsutil.DirMode.Perm()); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
		if err := root.Chmod(p, fsutil.DirMode); err != nil {
			return fmt.Errorf("set mode of %s: %w", p, err)
		}
	}
	return nil
}

// submitConfigApply records and runs the imported configuration step.
func submitConfigApply(
	ctx context.Context, db *store.DB, engine *jobs.Engine, inst *store.Instance, configs []ManifestConfig,
	merge bool, requestedBy string, afterFinish func(context.Context),
) (*store.Job, error) {
	id := inst.ID
	job, err := engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindConfigApply, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy,
		Payload: ConfigApplyPayload{Files: len(configs)},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim config_apply: %w", err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		jh.Progress(ctx, 10, "writing configuration")
		if err := ApplyManifestConfigs(ctx, db, inst, configs, merge); err != nil {
			return jobs.Failed(errcode.Internal, err)
		}
		jh.Progress(ctx, 100, "configuration written")
		return jobs.Outcome{Status: jobs.StatusSucceeded, AfterFinish: afterFinish}
	})
	if err != nil {
		return nil, fmt.Errorf("submit config_apply for instance %s: %w", id, err)
	}
	return job, nil
}

// settleIn settles the pending configs of one instance, in the config directory and at the
// root of the game installation. The config directory is settled with its subdirectories, the
// installation root on its own. A missing directory has nothing pending.
func settleIn(dataDir string) error {
	var errs []error
	for _, d := range []struct {
		open   func(string) (*os.Root, error)
		settle func(*os.Root) error
	}{
		{instance.OpenConfigDir, modconfig.SettlePendingTree},
		{openServerDir, modconfig.SettlePending},
	} {
		dir, err := d.open(dataDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, d.settle(dir))
		_ = dir.Close()
	}
	return errors.Join(errs...)
}

// openServerDir opens the root of the game installation.
func openServerDir(dataDir string) (*os.Root, error) {
	root, err := os.OpenRoot(instance.ServerDir(dataDir))
	if err != nil {
		return nil, fmt.Errorf("open server directory: %w", err)
	}
	return root, nil
}

// settleConfigs reapplies config edits made while the server ran, once it is down. A failure
// is logged on the job rather than failing it: the plugin's own values are still a valid file.
func settleConfigs(ctx context.Context, db *store.DB, jh *jobs.Handle, instanceID string) {
	inst, err := db.InstanceByID(ctx, instanceID)
	if err == nil && inst == nil {
		return
	}
	if err == nil {
		err = settleIn(inst.DataDir)
	}
	if err != nil {
		jh.Log(fmt.Sprintf("warning: config edits saved while the server ran could not be reapplied: %v", err))
		slog.WarnContext(ctx, "could not settle pending config edits",
			slog.String("instance_id", instanceID), slog.Any("error", err))
	}
}
