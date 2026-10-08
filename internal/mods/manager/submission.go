package manager

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// SubmitUninstall stages the removal set and submits its locked job. configs names the config
// files removed along with it.
func (i *Installer) SubmitUninstall(
	ctx context.Context, inst *store.Instance, names, configs []string, requestedBy string,
	audit *store.AuditEntry,
) (*store.Job, error) {
	root := stagingRoot(i.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		return nil, fmt.Errorf("create the mod staging root: %w", err)
	}
	staging, err := os.MkdirTemp(root, "uninstall-*")
	if err != nil {
		return nil, fmt.Errorf("create a staging directory for a mod uninstall: %w", err)
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	id := inst.ID
	payload := UninstallPayload{StagingDir: staging, FullNames: names, Configs: configs}
	job, err := i.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModUninstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: payload,
		Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim mod_uninstall for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, runUninstall(i.DB, inst, payload))
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve typed engine conflicts
	}
	submitted = true
	return job, nil
}

// SubmitToggle submits a locked job that changes one package's enabled state.
func (i *Installer) SubmitToggle(
	ctx context.Context, inst *store.Instance, fullName string, enable bool,
	requestedBy string, audit *store.AuditEntry,
) (*store.Job, error) {
	id := inst.ID
	payload := TogglePayload{FullName: fullName, Enable: enable}
	job, err := i.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModToggle, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: payload,
		Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim mod_toggle for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, runToggle(i.DB, inst, payload))
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve typed engine conflicts
	}
	return job, nil
}

// CheckResolvable reports whether the index can produce a closure for req. inst may describe
// an instance that does not exist yet, so an unresolvable request fails the create call rather
// than a job after the game download.
func (i *Installer) CheckResolvable(ctx context.Context, inst *store.Instance, req PackageRequest) error {
	prefer, _ := source.ByName(req.Source)
	idx := i.newIndex(ctx, inst.ID, prefer)
	_, resolveErr := i.planner().PlanInstall(ctx, inst, req.FullName, req.Version, idx)
	if idx.err != nil {
		return idx.err
	}
	return resolveErr
}

// SubmitInstall queues the minimum install of req for a definition chain, which follows the
// work through afterFinish.
func (i *Installer) SubmitInstall(
	ctx context.Context,
	inst *store.Instance,
	req PackageRequest,
	requestedBy string,
	afterFinish func(context.Context),
) (*store.Job, error) {
	return i.Submit(ctx, inst, &InstallPayload{
		FullName: req.FullName, Version: req.Version, Source: req.Source, Minimum: true,
	}, "install", requestedBy, nil, afterFinish)
}
