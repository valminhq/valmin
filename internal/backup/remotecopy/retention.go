package remotecopy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

func (h *Worker) MarkRetention(ctx context.Context) error {
	d, err := h.DB.RemoteDestination(ctx)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if d == nil || !d.Enabled {
		return nil
	}
	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	for i := range instances {
		inst := &instances[i]
		if inst.State == "deleting" {
			continue
		}
		if err := h.markInstanceRetention(ctx, inst, d.ID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Worker) submitCleanup(ctx context.Context, c *store.RemoteCopy) {
	_, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindRemotePrune, LockKey: LockKey,
		LockKeys: []string{"remote_instance:" + c.InstanceID}, InstanceID: &c.InstanceID, InstanceName: c.InstanceName,
		Payload: map[string]string{CopyIDField: c.ID},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxCheckRemoteCleanup(ctx, tx, c.ID); err != nil {
				return fmt.Errorf("claim remote cleanup: %w", err)
			}
			return nil
		},
	}, h.RunCleanup(c))
	if err != nil {
		var conflict *store.JobConflict
		if !errors.As(err, &conflict) && !errors.Is(err, jobs.ErrShuttingDown) &&
			!errors.Is(err, store.ErrRemoteUnavailable) {
			slog.ErrorContext(ctx, "submit remote retention", slog.Any("error", err))
		}
	}
}

func (h *Worker) RunCleanup(c *store.RemoteCopy) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		ctx, cancel := context.WithTimeout(ctx, time.Hour)
		defer cancel()
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, 10, "Removing expired remote backup")
		err := h.DeleteRemoteObjects(ctx, c)
		message := ""
		if err != nil {
			message = SafeError(err)
		}
		outcome := jobs.Outcome{Status: jobs.StatusSucceeded}
		if err != nil {
			outcome = Failed(message)
		}
		next := store.FormatTime(time.Now().Add(time.Hour))
		outcome.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
			return store.TxFinishRemoteCleanup(ctx, tx, c.ID, message, next)
		}
		return outcome
	}
}

func (h *Worker) DeleteRemoteObjects(ctx context.Context, c *store.RemoteCopy) error {
	d, err := h.DB.RemoteDestinationByID(ctx, c.DestinationID)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if d == nil || !d.Enabled || d.Retired {
		return remote.ErrConfiguration
	}
	b, err := h.BackendFor(d)
	if err != nil {
		return err
	}
	var archive, manifest remote.ObjectRef
	if json.Unmarshal([]byte(c.ObjectJSON), &archive) != nil ||
		json.Unmarshal([]byte(c.ManifestJSON), &manifest) != nil {
		return remote.ErrConfiguration
	}
	// A corrupt catalogue must never broaden a deletion to another key or directory.
	if archive.Key != remoteObjectKey(c) || manifest.Key != archive.Key+".json" {
		return remote.ErrConfiguration
	}
	if err := b.Delete(ctx, manifest); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if err := b.Delete(ctx, archive); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	return nil
}

func (h *Worker) markInstanceRetention(ctx context.Context, inst *store.Instance, destinationID string) error {
	copies, err := h.DB.RemoteRetentionCopies(ctx, inst.ID, destinationID)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	counts := [3]int{}
	limits := [3]int{inst.RemoteKeepCold, inst.RemoteKeepHot, inst.RemoteKeepSnapshots}
	for j := range copies {
		c := &copies[j]
		class := 0
		if c.Trigger == store.TriggerPreRestore || c.Trigger == store.TriggerPreUpdate ||
			c.Trigger == store.TriggerPreImport {
			class = 2
		} else if !c.Consistent {
			class = 1
		}
		counts[class]++
		if limits[class] > 0 && counts[class] > limits[class] {
			if err := h.DB.MarkRemoteCleanup(ctx, c.ID); err != nil {
				return fmt.Errorf("remote operation: %w", err)
			}
		}
	}
	return nil
}
