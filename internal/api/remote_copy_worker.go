package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

func (h *RemoteBackups) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	nextRetention := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.DB.ReconcileRemoteCopies(ctx); err != nil {
			slog.ErrorContext(ctx, "reconcile remote copies", slog.Any("error", err))
		}
		if time.Now().After(nextRetention) {
			if err := h.markRetention(ctx); err != nil {
				slog.ErrorContext(ctx, "select remote retention", slog.Any("error", err))
			} else {
				nextRetention = time.Now().Add(24 * time.Hour)
			}
		}
		h.dispatchRemote(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *RemoteBackups) dispatchRemote(ctx context.Context) {
	// Pending cleanup gets a turn before the next upload, without monopolizing failed destinations.
	cleanup, err := h.DB.DueRemoteCleanup(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "read remote cleanup", slog.Any("error", err))
		return
	}
	if len(cleanup) > 0 {
		h.submitCleanup(ctx, &cleanup[0])
		return
	}
	copies, err := h.DB.DueRemoteCopies(ctx, time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "read remote queue", slog.Any("error", err))
		return
	}
	if len(copies) == 0 {
		return
	}
	c := copies[0]
	_, err = h.Engine.Submit(ctx, &jobs.Spec{
		Kind:         jobs.KindRemoteCopy,
		LockKey:      remoteBackupLock,
		LockKeys:     []string{"remote_instance:" + c.InstanceID},
		InstanceID:   &c.InstanceID,
		InstanceName: c.InstanceName,
		Payload:      map[string]string{remoteCopyIDField: c.ID},
		OnClaim:      func(ctx context.Context, tx *sql.Tx) error { return store.TxClaimRemoteCopy(ctx, tx, c.ID) },
	}, h.runCopy(&c))
	if err != nil {
		var conflict *store.JobConflict
		if !errors.As(err, &conflict) && !errors.Is(err, store.ErrRemoteUnavailable) &&
			!errors.Is(err, jobs.ErrShuttingDown) {
			slog.ErrorContext(ctx, "submit remote copy", slog.Any("error", err))
		}
	}
}

func remoteObjectKey(c *store.RemoteCopy) string {
	return "valmin/" + c.DestinationID + "/" + c.InstanceID + "/" + c.BackupID + ".tar.gz"
}

func (h *RemoteBackups) runCopy(c *store.RemoteCopy) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		deadline, err := store.ParseTime(c.DeadlineAt)
		if err != nil {
			return remoteFailed("Remote copy deadline is invalid.")
		}
		deadline = minTime(deadline, time.Now().Add(time.Hour))
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		done := make(chan struct{})
		go h.watchCopy(attemptCtx, cancel, c, jh, done)
		jh.Progress(attemptCtx, 10, "Uploading remote backup")
		err = h.copyArchive(attemptCtx, c, jh)
		cancel()
		<-done
		return h.copyOutcome(ctx, c, jh, err)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (h *RemoteBackups) watchCopy(
	ctx context.Context,
	cancel context.CancelFunc,
	c *store.RemoteCopy,
	jh *jobs.Handle,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := h.DB.RemoteCopyByID(ctx, c.InstanceID, c.ID)
			if err != nil || current == nil || current.CancelRequested || jh.CancelRequested(ctx) {
				cancel()
				return
			}
		}
	}
}

func verifyRemoteSource(ctx context.Context, c *store.RemoteCopy) error {
	f, err := os.Open(c.SourcePath)
	if err != nil {
		return &remote.Failure{Message: "Local archive is no longer available."}
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, remoteSourceReader{ctx: ctx, reader: f})
	if ctx.Err() != nil {
		return &remote.Failure{Message: "Local archive verification was interrupted.", Temporary: true}
	}
	if err != nil || size != c.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != c.SHA256 {
		return &remote.Failure{Message: "Local archive checksum does not match its catalogue entry."}
	}
	return nil
}

func (h *RemoteBackups) copyArchive(ctx context.Context, c *store.RemoteCopy, jh *jobs.Handle) error {
	d, err := h.DB.RemoteDestinationByID(ctx, c.DestinationID)
	if err != nil {
		return fmt.Errorf("load remote destination: %w", err)
	}
	if d == nil || d.Retired || !d.Enabled {
		return remote.ErrConfiguration
	}
	b, err := h.backend(d)
	if err != nil {
		return err
	}
	if rclone, ok := b.(*remote.RcloneBackend); ok {
		if err := rclone.CheckConfig(ctx); err != nil {
			return fmt.Errorf("remote operation: %w", err)
		}
	}
	if err := verifyRemoteSource(ctx, c); err != nil {
		return err
	}
	object, err := b.Put(ctx, remoteObjectKey(c), c.SourcePath)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if err := h.recordObjects(ctx, c, object.Ref, remote.ObjectRef{}); err != nil {
		return err
	}
	info, err := b.Stat(ctx, object.Ref)
	if err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	if info.SizeBytes != c.SizeBytes {
		return &remote.Failure{Message: "Remote archive size did not match.", Temporary: true}
	}
	jh.Progress(ctx, 85, "Publishing backup manifest")
	manifest, err := h.putManifest(ctx, b, c)
	if err != nil {
		return err
	}
	return h.recordObjects(ctx, c, info.Ref, manifest.Ref)
}

func (h *RemoteBackups) recordObjects(
	ctx context.Context,
	c *store.RemoteCopy,
	object, manifest remote.ObjectRef,
) error {
	objectJSON, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("encode remote object: %w", err)
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode remote manifest: %w", err)
	}
	if err := h.DB.SaveRemoteObjects(ctx, c.ID, string(objectJSON), string(manifestJSON)); err != nil {
		return fmt.Errorf("remote operation: %w", err)
	}
	return nil
}

func (h *RemoteBackups) putManifest(ctx context.Context, b remote.Backend, c *store.RemoteCopy) (remote.Object, error) {
	payload := struct {
		Version      int    `json:"version"`
		BackupID     string `json:"backup_id"`
		InstanceID   string `json:"instance_id"`
		InstanceName string `json:"instance_name"`
		WorldName    string `json:"world_name"`
		CreatedAt    string `json:"created_at"`
		Consistent   bool   `json:"consistent"`
		Trigger      string `json:"trigger"`
		SizeBytes    int64  `json:"size_bytes"`
		SHA256       string `json:"sha256"`
	}{1, c.BackupID, c.InstanceID, c.InstanceName, c.WorldName, c.ArchiveCreatedAt, c.Consistent, c.Trigger, c.SizeBytes, c.SHA256}
	file, err := os.CreateTemp("", "valmin-remote-manifest-")
	if err != nil {
		return remote.Object{}, fmt.Errorf("create manifest: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := json.NewEncoder(file).Encode(payload); err != nil {
		_ = file.Close()
		return remote.Object{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return remote.Object{}, fmt.Errorf("close manifest: %w", err)
	}
	object, err := b.Put(ctx, remoteObjectKey(c)+".json", file.Name())
	if err != nil {
		return remote.Object{}, fmt.Errorf("remote operation: %w", err)
	}
	info, err := b.Stat(ctx, object.Ref)
	if err != nil {
		return remote.Object{}, fmt.Errorf("remote operation: %w", err)
	}
	if info.SizeBytes != object.SizeBytes {
		return remote.Object{}, &remote.Failure{Message: "Remote manifest size did not match.", Temporary: true}
	}
	return info, nil
}

func (h *RemoteBackups) copyOutcome(ctx context.Context, c *store.RemoteCopy, jh *jobs.Handle, err error) jobs.Outcome {
	status, message, next := "succeeded", "", store.Now()
	outcome := jobs.Outcome{Status: jobs.StatusSucceeded}
	if err != nil {
		status, message = "failed", safeRemoteError(err)
		current, loadErr := h.DB.RemoteCopyByID(ctx, c.InstanceID, c.ID)
		deadline, _ := store.ParseTime(c.DeadlineAt)
		switch {
		case loadErr == nil && current != nil && (current.CancelRequested || jh.CancelRequested(ctx)):
			status, message = "cancelled", "Remote upload cancelled."
		case ctx.Err() != nil || ((remote.Retryable(err) || errors.Is(err, context.DeadlineExceeded)) && time.Now().Before(deadline)):
			status = "retry_wait"
			next = store.FormatTime(time.Now().Add(remoteBackoff(c.Attempts + 1)))
		}
		outcome = remoteFailed(message)
		if status == "cancelled" {
			outcome.Status = jobs.StatusCancelled
		}
	}
	outcome.OnFinish = func(ctx context.Context, tx *sql.Tx) error {
		return store.TxFinishRemoteCopy(ctx, tx, c.ID, status, message, next)
	}
	if status == "succeeded" {
		outcome.AfterFinish = func(ctx context.Context) {
			if err := h.markRetention(ctx); err != nil {
				slog.ErrorContext(ctx, "select remote retention", slog.Any("error", err))
			}
		}
	}
	return outcome
}

func remoteBackoff(attempt int) time.Duration {
	base := min(time.Minute*time.Duration(1<<min(max(attempt-1, 0), 6)), time.Hour)
	jitter := time.Duration(rand.Int64N(int64(base/5) + 1)) //nolint:gosec // Retry jitter is not a security token.
	return min(base+jitter, time.Hour)
}

type remoteSourceReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r remoteSourceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read remote source: %w", err)
	}
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read remote source: %w", err)
	}
	return n, nil
}
