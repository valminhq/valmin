package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	modconfig "github.com/valminhq/valmin/internal/mods/config"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// CheckManifestConfigName accepts one plain .cfg filename.
func CheckManifestConfigName(name string) error {
	if name == "" {
		return fmt.Errorf("a config entry has no filename")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not a plain filename", name)
	}
	if !strings.HasSuffix(name, ".cfg") {
		return fmt.Errorf("%q is not a .cfg file", name)
	}
	return nil
}

// ApplyManifestConfigs writes the configuration files of an imported definition. With merge
// set, each file's settings are applied over the file already on disk instead of replacing it.
// The bytes each write replaces are kept as a panel edit keeps them.
func ApplyManifestConfigs(inst *store.Instance, configs []ManifestConfig, merge bool) error {
	if len(configs) == 0 {
		return nil
	}
	for _, cfg := range configs {
		if err := CheckManifestConfigName(cfg.File); err != nil {
			return err
		}
	}
	if err := mkdirConfigDir(inst.DataDir); err != nil {
		return err
	}
	dir, err := instance.OpenConfigDir(inst.DataDir)
	if err != nil {
		return err //nolint:wrapcheck // OpenConfigDir names the directory
	}
	defer func() { _ = dir.Close() }()
	for _, cfg := range configs {
		current, _, err := fsutil.ReadRegularIn(dir, cfg.File)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read config %s: %w", cfg.File, err)
		}
		next := []byte(cfg.Content)
		if merge {
			next = modconfig.Merge(current, next)
		}
		if err := modconfig.KeepCopies(dir, cfg.File, current); err != nil {
			return err //nolint:wrapcheck // KeepCopies names the file
		}
		if err := fsutil.WriteFileAtomicIn(dir, cfg.File, next); err != nil {
			return fmt.Errorf("write config %s: %w", cfg.File, err)
		}
	}
	return nil
}

// mkdirConfigDir creates the config directory inside the server tree, with the exact mode
// fsutil.MkdirAllExact gives, without following a symlink out of the tree.
func mkdirConfigDir(dataDir string) error {
	server, err := os.OpenRoot(instance.ServerDir(dataDir))
	if err != nil {
		return fmt.Errorf("open server directory: %w", err)
	}
	defer func() { _ = server.Close() }()
	var p string
	for _, part := range strings.Split(instance.ConfigDir, "/") {
		p = filepath.Join(p, part)
		if _, err := server.Lstat(p); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", p, err)
		}
		if err := server.Mkdir(p, fsutil.DirMode); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
		if err := server.Chmod(p, fsutil.DirMode); err != nil {
			return fmt.Errorf("set mode of %s: %w", p, err)
		}
	}
	return nil
}

// submitConfigApply records and runs the imported configuration step.
func submitConfigApply(
	ctx context.Context, engine *jobs.Engine, inst *store.Instance, configs []ManifestConfig,
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
		if err := ApplyManifestConfigs(inst, configs, merge); err != nil {
			return jobs.Outcome{Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error()}
		}
		jh.Progress(ctx, 100, "configuration written")
		return jobs.Outcome{Status: jobs.StatusSucceeded, AfterFinish: afterFinish}
	})
	if err != nil {
		return nil, fmt.Errorf("submit config_apply for instance %s: %w", id, err)
	}
	return job, nil
}

// settleIn settles the pending configs of one instance. One without a config directory has
// nothing pending.
func settleIn(dataDir string) error {
	dir, err := instance.OpenConfigDir(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err //nolint:wrapcheck // OpenConfigDir names the directory
	}
	defer func() { _ = dir.Close() }()
	return modconfig.SettlePending(dir) //nolint:wrapcheck // SettlePending names the file
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
