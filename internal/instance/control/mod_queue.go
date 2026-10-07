package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// ModQueue applies the mod installs queued while a server ran, one at a time once it is
// stopped, then gives a server a restart stopped for them the start it is owed.
type ModQueue struct {
	DB        *store.DB
	Authz     *authz.Authz
	Installer *manager.Installer
	Starter   *Starter
}

// Drain submits the next step for every instance with a queue. An instance that is busy or
// not stopped is left for a later pass.
func (q *ModQueue) Drain(ctx context.Context) {
	ids, err := q.DB.InstancesWithModQueue(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read the mod install queue", slog.Any("error", err))
		return
	}
	for _, id := range ids {
		if err := q.step(ctx, id); err != nil {
			slog.WarnContext(ctx, "mod install queue step failed, will retry",
				slog.String("instance_id", id), slog.Any("error", err))
		}
	}
}

func (q *ModQueue) step(ctx context.Context, id string) error {
	inst, err := q.DB.InstanceByID(ctx, id)
	if err != nil || inst == nil {
		return err //nolint:wrapcheck // the store already names the instance
	}
	if st := instance.State(inst.State); st != instance.StateStopped {
		if st == instance.StateRunning || st == instance.StateStarting {
			// Started by someone else: the owed start is spent, the installs wait for the next stop.
			return q.DB.ClearQueuedModStart(ctx, id) //nolint:wrapcheck // the store names the instance
		}
		return nil
	}
	if op, err := q.DB.OpenOperation(ctx, id); err != nil || op != nil {
		return err //nolint:wrapcheck // the store names the instance
	}

	queued, err := q.DB.QueuedModInstalls(ctx, id)
	if err != nil {
		return err //nolint:wrapcheck // the store names the instance
	}
	if len(queued) > 0 {
		return q.installNext(ctx, inst, &queued[0])
	}
	return q.startOwed(ctx, inst)
}

// installNext submits one queued install and takes it off the queue. The install runs on the
// authority of whoever queued it, so that is checked again now: an install queued by a user
// since deleted, disabled or stripped of mods.manage is dropped instead.
func (q *ModQueue) installNext(ctx context.Context, inst *store.Instance, next *store.QueuedModInstall) error {
	allowed, err := q.mayInstall(ctx, inst.ID, next.RequestedBy)
	if err != nil {
		return err
	}
	if !allowed {
		slog.WarnContext(ctx, "dropped a queued mod install its requester may no longer make",
			slog.String("instance_id", inst.ID), slog.String("user_id", next.RequestedBy),
			slog.String("full_name", next.FullName))
		_, err := q.DB.UnqueueModInstall(ctx, inst.ID, next.FullName)
		return err //nolint:wrapcheck // the store names the package
	}
	audit, err := q.audit(ctx, next)
	if err != nil {
		return err
	}
	_, err = q.Installer.Submit(ctx, inst, &manager.InstallPayload{
		FullName: next.FullName, Version: next.Version, Source: next.Source,
	}, "install", next.RequestedBy, audit, nil)
	if busy(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("submit queued install of %s: %w", next.FullName, err)
	}
	_, err = q.DB.UnqueueModInstall(ctx, inst.ID, next.FullName)
	return err //nolint:wrapcheck // the store names the package
}

// startOwed starts a server a restart stopped for the queue, once nothing is left in it.
func (q *ModQueue) startOwed(ctx context.Context, inst *store.Instance) error {
	owed, by, err := q.DB.QueuedModStart(ctx, inst.ID)
	if err != nil || !owed {
		return err //nolint:wrapcheck // the store names the instance
	}
	if inst.ContainerID != nil {
		start := &StartSubmission{Instance: inst, ContainerID: *inst.ContainerID, RequestedBy: by}
		_, err = q.Starter.Submit(ctx, start)
		if busy(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return q.DB.ClearQueuedModStart(ctx, inst.ID) //nolint:wrapcheck // the store names the instance
}

// mayInstall reports whether userID still holds mods.manage on the instance.
func (q *ModQueue) mayInstall(ctx context.Context, instanceID, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	u, err := q.DB.UserByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("load user %s: %w", userID, err)
	}
	return q.Authz.Can(ctx, u, authz.ModsManage, instanceID), nil
}

// audit is the install's trail entry, credited to whoever queued it.
func (q *ModQueue) audit(ctx context.Context, next *store.QueuedModInstall) (*store.AuditEntry, error) {
	from, installedFrom, installed, err := q.DB.InstanceModVersion(ctx, next.InstanceID, next.FullName)
	if err != nil {
		return nil, fmt.Errorf("read the installed version of %s: %w", next.FullName, err)
	}
	src := next.Source
	if src == "" && installed {
		src = installedFrom.String()
	}
	detail, err := json.Marshal(struct {
		FullName string `json:"full_name"`
		From     string `json:"from,omitempty"`
		To       string `json:"to"`
		Source   string `json:"source"`
	}{next.FullName, from, next.Version, src})
	if err != nil {
		return nil, fmt.Errorf("encode audit detail: %w", err)
	}
	return &store.AuditEntry{
		UserID: next.RequestedBy, InstanceID: next.InstanceID,
		Action: "instances.mods.install", Detail: string(detail),
	}, nil
}

// busy reports a submission refused because another job holds the instance.
func busy(err error) bool {
	var conflict *store.JobConflict
	return errors.As(err, &conflict)
}
