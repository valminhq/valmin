package control

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
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

// ApplyManifestConfigs writes the configuration files of an imported definition.
func ApplyManifestConfigs(inst *store.Instance, configs []ManifestConfig) error {
	if len(configs) == 0 {
		return nil
	}
	dir := filepath.Join(instance.ServerDir(inst.DataDir), filepath.FromSlash(instance.ConfigDir))
	if err := fsutil.MkdirAllExact(dir); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	for _, cfg := range configs {
		if err := CheckManifestConfigName(cfg.File); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, cfg.File), []byte(cfg.Content)); err != nil {
			return fmt.Errorf("write config %s: %w", cfg.File, err)
		}
	}
	return nil
}

// SubmitConfigApply records and runs the imported configuration step.
func SubmitConfigApply(
	ctx context.Context, engine *jobs.Engine, inst *store.Instance, configs []ManifestConfig,
	requestedBy string, afterFinish func(context.Context),
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
		if err := ApplyManifestConfigs(inst, configs); err != nil {
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
