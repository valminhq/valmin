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

// MarkRetention marks for cleanup the copies each instance's remote retention no longer keeps,
// takes back marks on copies it keeps again, and marks failed uploads that left an archive
// behind. An instance that fails is reported and does not stop the others.
func (w *Worker) MarkRetention(ctx context.Context) error {
	d, err := w.DB.RemoteDestination(ctx)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if d == nil || !d.Enabled {
		return nil
	}
	instances, err := w.DB.ListInstances(ctx, nil)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	var errs []error
	for i := range instances {
		inst := &instances[i]
		if inst.State == "deleting" {
			continue
		}
		if err := w.markInstanceRetention(ctx, inst, d.ID); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) submitCleanup(ctx context.Context, c *store.RemoteCopy) {
	_, err := w.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindRemotePrune, LockKey: LockKey,
		LockKeys: []string{"remote_instance:" + c.InstanceID}, InstanceID: &c.InstanceID, InstanceName: c.InstanceName,
		Payload: map[string]string{CopyIDField: c.ID},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			if err := store.TxCheckRemoteCleanup(ctx, tx, c.ID); err != nil {
				return fmt.Errorf("claim remote cleanup: %w", err)
			}
			return nil
		},
	}, w.RunCleanup(c))
	if err != nil {
		var conflict *store.JobConflict
		if !errors.As(err, &conflict) && !errors.Is(err, jobs.ErrShuttingDown) &&
			!errors.Is(err, store.ErrRemoteUnavailable) {
			slog.ErrorContext(ctx, "submit remote retention", slog.Any("error", err))
		}
	}
}

func (w *Worker) RunCleanup(c *store.RemoteCopy) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		ctx, cancel := context.WithTimeout(ctx, time.Hour)
		defer cancel()
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, 10, "Removing expired remote backup")
		err := w.DeleteRemoteObjects(ctx, c)
		message := ""
		if err != nil {
			message = safeError(err)
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

func (w *Worker) DeleteRemoteObjects(ctx context.Context, c *store.RemoteCopy) error {
	d, err := w.DB.RemoteDestinationByID(ctx, c.DestinationID)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if d == nil || !d.Enabled || d.Retired {
		return remote.ErrConfiguration
	}
	b, err := w.BackendFor(d)
	if err != nil {
		return err
	}
	var archive, manifest remote.ObjectRef
	if json.Unmarshal([]byte(c.ObjectJSON), &archive) != nil ||
		json.Unmarshal([]byte(c.ManifestJSON), &manifest) != nil {
		return remote.ErrConfiguration
	}
	// A corrupt catalogue must never broaden a deletion to another key or directory. A copy
	// that failed after its upload may have no manifest; it then loses only its archive.
	abandoned := c.Status != store.RemoteCopySucceeded && manifest.Key == ""
	if archive.Key != remoteObjectKey(c) || (!abandoned && manifest.Key != archive.Key+".json") {
		return remote.ErrConfiguration
	}
	if !abandoned {
		if err := b.Delete(ctx, manifest); err != nil {
			return fmt.Errorf("remote operation: %w", err)
		}
	}
	if err := b.Delete(ctx, archive); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	return nil
}

// retentionClass is how retention counts a copy: cold archives taken with the server stopped,
// hot ones taken while it ran, and snapshots of a world about to be replaced.
type retentionClass int

const (
	classCold retentionClass = iota
	classHot
	classSnapshot
)

func classOf(trigger string, consistent bool) retentionClass {
	switch {
	case store.SnapshotTrigger(trigger):
		return classSnapshot
	case !consistent:
		return classHot
	default:
		return classCold
	}
}

// keepLimit is how many remote copies of a class an instance keeps; zero keeps every one.
func keepLimit(inst *store.Instance, class retentionClass) int {
	switch class {
	case classHot:
		return inst.RemoteKeepHot
	case classSnapshot:
		return inst.RemoteKeepSnapshots
	case classCold:
		return inst.RemoteKeepCold
	}
	return inst.RemoteKeepCold
}

func (w *Worker) markInstanceRetention(ctx context.Context, inst *store.Instance, destinationID string) error {
	if err := w.DB.MarkAbandonedRemoteUploads(ctx, inst.ID, destinationID); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	copies, err := w.DB.RemoteRetentionCopies(ctx, inst.ID, destinationID)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	counts := map[retentionClass]int{}
	for j := range copies {
		c := &copies[j]
		class := classOf(c.Trigger, c.Consistent)
		counts[class]++
		limit := keepLimit(inst, class)
		switch {
		case limit > 0 && counts[class] > limit:
			err = w.DB.MarkRemoteCleanup(ctx, c.ID)
		case c.CleanupPending:
			err = w.DB.UnmarkRemoteCleanup(ctx, c.ID)
		}
		if err != nil {
			return fmt.Errorf("remote operation: %w", err)
		}
	}
	return nil
}

// PrunedOnArrival reports whether a copy of b would be removed by retention as soon as it
// arrived, because the destination already keeps as many newer copies of its class as the
// instance allows.
func PrunedOnArrival(ctx context.Context, db *store.DB, inst *store.Instance, b *store.Backup) (bool, error) {
	class := classOf(b.Trigger, b.Consistent)
	limit := keepLimit(inst, class)
	if limit <= 0 {
		return false, nil
	}
	d, err := db.RemoteDestination(ctx)
	if err != nil {
		return false, fmt.Errorf("read remote destination: %w", err)
	}
	if d == nil {
		return false, nil
	}
	copies, err := db.RemoteRetentionCopies(ctx, inst.ID, d.ID)
	if err != nil {
		return false, fmt.Errorf("read remote copies: %w", err)
	}
	newer := 0
	created := store.FormatTime(b.CreatedAt)
	for j := range copies {
		c := &copies[j]
		if c.BackupID != b.ID && !c.CleanupPending && classOf(c.Trigger, c.Consistent) == class &&
			c.ArchiveCreatedAt > created {
			newer++
		}
	}
	return newer >= limit, nil
}
