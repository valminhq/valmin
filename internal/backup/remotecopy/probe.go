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
	if rclone, ok := b.(*remote.RcloneBackend); ok {
		if err := rclone.CheckConfig(ctx); err != nil {
			return fmt.Errorf("remote operation: %w", err)
		}
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
		return fmt.Errorf("remote operation: %w", err)
	}
	info, statErr := b.Stat(ctx, object.Ref)
	deleteErr := b.Delete(ctx, object.Ref)
	if statErr != nil {
		return fmt.Errorf("remote operation: %w", statErr)
	}
	if info.SizeBytes != object.SizeBytes {
		return &remote.Failure{Message: "Remote probe size did not match."}
	}
	if deleteErr != nil {
		return fmt.Errorf("delete remote probe: %w", deleteErr)
	}
	return nil
}
