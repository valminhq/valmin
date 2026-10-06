package manager

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// SubmitUninstall stages the removal set and submits its locked job.
func (i *Installer) SubmitUninstall(
	ctx context.Context, inst *store.Instance, names []string, requestedBy string, audit *store.AuditEntry,
) (*store.Job, error) {
	root := stagingRoot(i.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		return nil, err //nolint:wrapcheck // preserve staging failure text
	}
	staging, err := os.MkdirTemp(root, "uninstall-*")
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve staging failure text
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	id := inst.ID
	payload := UninstallPayload{StagingDir: staging, FullNames: names}
	job, err := i.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModUninstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: payload,
		Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := holdStoppedTx(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("claim mod_uninstall for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, RunUninstall(i.DB, inst, payload))
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
			ok, err := holdStoppedTx(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("claim mod_toggle for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, RunToggle(i.DB, inst, payload))
	if err != nil {
		return nil, err //nolint:wrapcheck // preserve typed engine conflicts
	}
	return job, nil
}
