package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// ModQueue applies the mod installs queued while a server ran once it is stopped, then gives a
// server a restart stopped for them the start it is owed. Queued updates from one requester run
// as one job, so the world is archived once and the updates resolve together.
type ModQueue struct {
	DB        *store.DB
	Authz     *authz.Authz
	Installer *manager.Installer
	Starter   *Starter
	// PublishMods tells clients an instance's mods or queue changed. It may be nil.
	PublishMods func(instanceID string)
	// inFlight holds the instances with a queued job submitted and not yet seen finished. Only
	// Drain touches it, and Drain runs on the supervisor's goroutine alone.
	inFlight map[string]bool
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
	q.announceFinished(ctx)
}

// announceFinished publishes once for each instance whose queued job has released its lock with
// nothing submitted after it.
func (q *ModQueue) announceFinished(ctx context.Context) {
	if len(q.inFlight) == 0 {
		return
	}
	held, err := q.DB.HeldLockKeys(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read held locks for the mod queue", slog.Any("error", err))
		return
	}
	for id := range q.inFlight {
		if !held[jobs.InstanceLockKey(id)] {
			delete(q.inFlight, id)
			q.publish(id)
		}
	}
}

// submitted records a queued job submitted for instanceID and announces the queue change.
func (q *ModQueue) submitted(instanceID string) {
	if q.inFlight == nil {
		q.inFlight = map[string]bool{}
	}
	q.inFlight[instanceID] = true
	q.publish(instanceID)
}

func (q *ModQueue) publish(instanceID string) {
	if q.PublishMods != nil {
		q.PublishMods(instanceID)
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
	// A server stopped ahead of a power cut keeps its queue until after it.
	if soon, err := q.DB.PowerCutWithin(ctx, time.Now().UTC(), scheduler.ShutdownQuietLead); err != nil || soon {
		return err //nolint:wrapcheck // the store names what it read
	}

	queued, err := q.DB.QueuedModInstalls(ctx, id)
	if err != nil {
		return err //nolint:wrapcheck // the store names the instance
	}
	if len(queued) == 0 {
		return q.startOwed(ctx, inst)
	}
	targets, err := q.updateBatch(ctx, id, queued)
	if err != nil {
		return err
	}
	if len(targets) < 2 {
		return q.installNext(ctx, inst, &queued[0])
	}
	return q.installUpdates(ctx, inst, &queued[0], targets)
}

// updateBatch returns the queued updates that can run as one job with the head of the queue.
// An entry qualifies when the head's requester queued it, Update all would offer that mod, and
// the version is still a valid update. The result is empty when the head itself doesn't qualify,
// so a new install, a downgrade or a modpack runs alone under the install rules.
func (q *ModQueue) updateBatch(
	ctx context.Context, instanceID string, queued []store.QueuedModInstall,
) ([]manager.UpdateTarget, error) {
	planner := q.Installer.Planner()
	pending, err := planner.PendingUpdates(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read pending updates: %w", err)
	}
	offered := make(map[string]bool, len(pending))
	for _, p := range pending {
		offered[p.FullName] = true
	}
	head := queued[0]
	if !offered[head.FullName] {
		return nil, nil
	}
	var candidates []manager.UpdateTarget
	for _, e := range queued {
		if e.RequestedBy != head.RequestedBy || !offered[e.FullName] {
			continue
		}
		src := e.Source
		if src == "" {
			_, installedFrom, _, err := q.DB.InstanceModVersion(ctx, instanceID, e.FullName)
			if err != nil {
				return nil, fmt.Errorf("read the installed source of %s: %w", e.FullName, err)
			}
			src = installedFrom.String()
		}
		candidates = append(candidates, manager.UpdateTarget{FullName: e.FullName, Source: src, Version: e.Version})
	}
	checked, _, err := planner.CheckUpdateTargets(ctx, instanceID, candidates)
	if err != nil {
		return nil, fmt.Errorf("check queued updates: %w", err)
	}
	if !slices.ContainsFunc(checked, func(t manager.UpdateTarget) bool { return t.FullName == head.FullName }) {
		return nil, nil
	}
	return checked, nil
}

// installUpdates submits targets as one update job credited to head's requester and takes them
// off the queue. The requester's authority is checked again first, as installNext does.
func (q *ModQueue) installUpdates(
	ctx context.Context, inst *store.Instance, head *store.QueuedModInstall, targets []manager.UpdateTarget,
) error {
	allowed, err := q.mayInstall(ctx, inst.ID, head.RequestedBy)
	if err != nil {
		return err
	}
	if !allowed {
		return q.drop(ctx, inst.ID, head)
	}
	audit, err := updateAudit(inst.ID, head.RequestedBy, targets)
	if err != nil {
		return err
	}
	_, err = q.Installer.Submit(ctx, inst, &manager.InstallPayload{Updates: targets, Backup: true},
		"update", head.RequestedBy, audit, nil)
	if busy(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("submit %d queued updates: %w", len(targets), err)
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.FullName
	}
	if err := q.DB.UnqueueModInstalls(ctx, inst.ID, names); err != nil {
		return err //nolint:wrapcheck // the store names the instance
	}
	q.submitted(inst.ID)
	return nil
}

// updateAudit is the trail entry of a batched update, the shape POST /mods/updates records.
func updateAudit(instanceID, requestedBy string, targets []manager.UpdateTarget) (*store.AuditEntry, error) {
	type change struct {
		FullName string `json:"full_name"`
		From     string `json:"from"`
		To       string `json:"to"`
	}
	packages := make([]change, len(targets))
	for i, t := range targets {
		packages[i] = change{FullName: t.FullName, From: t.FromVersion, To: t.Version}
	}
	detail, err := json.Marshal(map[string]any{"packages": packages})
	if err != nil {
		return nil, fmt.Errorf("encode audit detail: %w", err)
	}
	return &store.AuditEntry{
		UserID: requestedBy, InstanceID: instanceID,
		Action: "instances.mods.update", Detail: string(detail),
	}, nil
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
		return q.drop(ctx, inst.ID, next)
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
	if _, err := q.DB.UnqueueModInstall(ctx, inst.ID, next.FullName); err != nil {
		return err //nolint:wrapcheck // the store names the package
	}
	q.submitted(inst.ID)
	return nil
}

// drop takes off the queue an install its requester may no longer make.
func (q *ModQueue) drop(ctx context.Context, instanceID string, entry *store.QueuedModInstall) error {
	slog.WarnContext(ctx, "dropped a queued mod install its requester may no longer make",
		slog.String("instance_id", instanceID), slog.String("user_id", entry.RequestedBy),
		slog.String("full_name", entry.FullName))
	if _, err := q.DB.UnqueueModInstall(ctx, instanceID, entry.FullName); err != nil {
		return err //nolint:wrapcheck // the store names the package
	}
	q.publish(instanceID)
	return nil
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
