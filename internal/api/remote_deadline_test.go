package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// blockingRemoteBackend models provider calls that reach cancellation or a deadline.
type blockingRemoteBackend struct {
	entered   chan struct{}
	deleteErr error
}

func (b *blockingRemoteBackend) Put(ctx context.Context, _, _ string) (remote.Object, error) {
	close(b.entered)
	<-ctx.Done()
	return remote.Object{}, ctx.Err()
}

func (*blockingRemoteBackend) Stat(context.Context, remote.ObjectRef) (remote.Object, error) {
	return remote.Object{}, nil
}

func (b *blockingRemoteBackend) Delete(ctx context.Context, _ remote.ObjectRef) error {
	close(b.entered)
	if b.deleteErr != nil {
		return b.deleteErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func waitRemoteBackend(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("remote backend did not begin the operation")
	}
}

func TestRemoteDestinationTestPersistsResultAfterTransferCancellation(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)
	saveRemoteDestination(t, db)
	backend := &blockingRemoteBackend{entered: make(chan struct{})}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) {
		return backend, nil
	}
	destination, err := db.RemoteDestination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remoteBackups := rt.RemoteBackups()
	job, err := remoteBackups.Engine.Submit(t.Context(), &jobs.Spec{
		Kind: jobs.KindRemoteTest, LockKey: remoteBackupLock,
	}, func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		// Model a provider transfer context expiring while the job itself remains finishable.
		transferCtx, cancel := context.WithCancel(ctx)
		cancel()
		return remoteBackups.runTest(destination)(transferCtx, jh)
	})
	if err != nil {
		t.Fatalf("submit remote test: %v", err)
	}

	waitRemoteBackend(t, backend.entered)
	finished := waitJob(t, rt, admin, job.ID)
	if finished.Status != jobs.StatusFailed {
		t.Fatalf("remote test status = %q, want failed after transfer cancellation", finished.Status)
	}
	got, err := db.RemoteDestination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.LastTestAt == nil || got.LastTestError == "" {
		t.Errorf("cancelled remote test result = %+v, want persisted timestamp and error", got)
	}
}

func TestRemoteRetentionPersistsCleanupResultAfterTransferCancellation(t *testing.T) {
	rt, db, root, admin, _ := backupsWorld(t)
	remoteCopy := queueCopyForWorker(t, db, root, "cleanup-cancel")
	archive, err := json.Marshal(remote.ObjectRef{Key: "valmin/remote-destination/inst-a/cleanup-cancel.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(remote.ObjectRef{Key: "valmin/remote-destination/inst-a/cleanup-cancel.tar.gz.json"})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE remote_copies SET status='succeeded',cleanup_pending=TRUE,
	 object_json=?,manifest_json=? WHERE id=?`, string(archive), string(manifest), remoteCopy.ID)
	remoteCopy, err = db.RemoteCopyByID(t.Context(), remoteCopy.InstanceID, remoteCopy.ID)
	if err != nil {
		t.Fatal(err)
	}
	backend := &blockingRemoteBackend{entered: make(chan struct{}), deleteErr: context.DeadlineExceeded}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) {
		return backend, nil
	}
	instanceID := remoteCopy.InstanceID
	job, err := rt.RemoteBackups().Engine.Submit(t.Context(), &jobs.Spec{
		Kind: jobs.KindRemotePrune, LockKey: remoteBackupLock,
		InstanceID: &instanceID, InstanceName: remoteCopy.InstanceName,
	}, rt.RemoteBackups().runCleanup(remoteCopy))
	if err != nil {
		t.Fatalf("submit remote cleanup: %v", err)
	}

	waitRemoteBackend(t, backend.entered)
	finished := waitJob(t, rt, admin, job.ID)
	if finished.Status != jobs.StatusFailed {
		t.Fatalf("remote cleanup status = %q, want failed after transfer cancellation", finished.Status)
	}
	got, err := db.RemoteCopyByID(t.Context(), remoteCopy.InstanceID, remoteCopy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CleanupPending || got.CleanupError == "" || got.CleanupNextAt == "" {
		t.Errorf("cancelled remote cleanup result = %+v, want pending retry and persisted error", got)
	}
}
