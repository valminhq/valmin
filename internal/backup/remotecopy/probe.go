package remotecopy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Probe checks a configured destination and records the result with its job.
func (w *Worker) Probe(d *store.RemoteDestination) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		jh.Progress(ctx, 10, "Testing remote storage")
		b, err := w.BackendFor(d)
		if err == nil {
			err = probe(ctx, b, d.ID)
		}
		message := ""
		if err != nil {
			message = safeError(err)
		}
		outcome := jobs.Outcome{Status: jobs.StatusSucceeded}
		if err != nil {
			outcome = Failed(message)
		}
		outcome.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
			return store.TxRecordRemoteTest(ctx, tx, d.ID, message)
		}
		return outcome
	}
}

func probe(ctx context.Context, b remote.Backend, id string) error {
	if err := b.Check(ctx); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	dir, err := os.MkdirTemp("", "valmin-remote-probe-")
	if err != nil {
		return fmt.Errorf("create probe directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "probe")
	if err := os.WriteFile(file, []byte("valmin remote backup probe\n"), 0o600); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	key := "valmin/" + id + "/probes/" + store.NewID()
	object, err := b.Put(ctx, key, file)
	if err != nil {
		// The upload may have landed before the failure, so the object is removed anyway.
		_ = deleteProbe(ctx, b, remote.ObjectRef{Key: key})
		return fmt.Errorf("remote operation: %w", err)
	}
	info, statErr := b.Stat(ctx, object.Ref)
	deleteErr := deleteProbe(ctx, b, object.Ref)
	if statErr != nil {
		return fmt.Errorf("remote operation: %w", statErr)
	}
	if info.SizeBytes != object.SizeBytes {
		return &remote.Failure{Message: "Remote probe size did not match."}
	}
	if deleteErr != nil {
		return deleteErr
	}
	return nil
}

// probeCleanupTimeout bounds removing the probe object once the probe's own deadline is spent.
const probeCleanupTimeout = 30 * time.Second

// deleteProbe removes the probe object on a context of its own, so a probe that used up its
// deadline still cleans up.
func deleteProbe(ctx context.Context, b remote.Backend, ref remote.ObjectRef) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeCleanupTimeout)
	defer cancel()
	if err := b.Delete(ctx, ref); err != nil {
		return fmt.Errorf("delete remote probe: %w", err)
	}
	return nil
}
