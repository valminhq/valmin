package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/backup/remote"
	"github.com/valminhq/valmin/internal/store"
)

func saveRemoteDestination(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.SaveRemoteDestination(t.Context(), &store.RemoteDestination{
		ID: "remote-destination", Kind: "rclone", Enabled: true,
		Credentials: "credential-envelope-secret", RemoteName: "archive", Folder: "valmin",
	}, nil)
	if err != nil {
		t.Fatalf("SaveRemoteDestination: %v", err)
	}
}

func TestRemoteBackupAuthorizationAndCredentialRedaction(t *testing.T) {
	rt, db, root, admin, member := backupsWorld(t)
	saveRemoteDestination(t, db)
	seedArchive(t, db, root, "b-remote", store.TriggerManual, true, time.Now().UTC())

	adminResponse := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/remote-backup-destination", http.NoBody))
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("admin destination read = %d, want 200 (%s)", adminResponse.Code, adminResponse.Body)
	}
	body := adminResponse.Body.String()
	if !strings.Contains(body, `"has_credentials":true`) || strings.Contains(body, "credential-envelope-secret") ||
		strings.Contains(body, `"credentials"`) {
		t.Errorf("destination response does not safely report credential presence: %s", body)
	}

	memberResponse := as(rt, member, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/remote-backup-destination", http.NoBody))
	if memberResponse.Code != http.StatusNotFound {
		t.Errorf("member destination read = %d, want 404 (%s)", memberResponse.Code, memberResponse.Body)
	}
	upload := as(rt, member, httptest.NewRequest(http.MethodPost,
		"/api/v1/instances/inst-a/backups/b-remote/remote-copy", http.NoBody))
	if upload.Code != http.StatusForbidden {
		t.Errorf("viewer remote-copy request = %d, want 403 (%s)", upload.Code, upload.Body)
	}
}

func TestRemoteDestinationTestSucceedsWithInjectedBackend(t *testing.T) {
	rt, db, _, admin, _ := backupsWorld(t)
	saveRemoteDestination(t, db)
	fake := &remoteBackupBackend{objects: make(map[string][]byte)}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) { return fake, nil }

	response := as(rt, admin, httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/remote-backup-destination/test", http.NoBody))
	if response.Code != http.StatusAccepted {
		t.Fatalf("destination test = %d, want 202 (%s)", response.Code, response.Body)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	decodeInto(t, response, &accepted)
	result := waitJob(t, rt, admin, accepted.JobID)
	if result.Status != "succeeded" {
		t.Fatalf("destination test job = %+v, want success", result)
	}
	fake.mu.Lock()
	puts, stats, deletes := fake.puts, fake.stats, fake.deletes
	fake.mu.Unlock()
	if puts != 1 || stats != 1 || deletes != 1 {
		t.Errorf("fake backend calls = put:%d stat:%d delete:%d, want one each", puts, stats, deletes)
	}
	destination, err := db.RemoteDestination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if destination == nil || destination.LastTestAt == nil || destination.LastTestError != "" {
		t.Errorf("recorded destination test = %+v, want a successful timestamp and no error", destination)
	}
}

func TestRemoteRetentionKeepsClassesSeparate(t *testing.T) {
	rt, db, root, _, _ := backupsWorld(t)
	saveRemoteDestination(t, db)
	seed(t, db, `UPDATE instances SET remote_keep_cold=1, remote_keep_hot=1, remote_keep_snapshots=1 WHERE id='inst-a'`)

	type retainedCopy struct {
		id         string
		trigger    string
		consistent bool
		at         time.Time
	}
	now := time.Now().UTC()
	copies := []retainedCopy{
		{id: "cold-old", trigger: store.TriggerScheduled, consistent: true, at: now.Add(-6 * time.Hour)},
		{id: "cold-new", trigger: store.TriggerScheduled, consistent: true, at: now.Add(-5 * time.Hour)},
		{id: "hot-old", trigger: store.TriggerManual, consistent: false, at: now.Add(-4 * time.Hour)},
		{id: "hot-new", trigger: store.TriggerManual, consistent: false, at: now.Add(-3 * time.Hour)},
		{id: "snapshot-old", trigger: store.TriggerPreRestore, consistent: true, at: now.Add(-2 * time.Hour)},
		{id: "snapshot-new", trigger: store.TriggerPreRestore, consistent: true, at: now.Add(-time.Hour)},
		{id: "failed", trigger: store.TriggerScheduled, consistent: true, at: now},
	}
	for _, backup := range copies {
		seedArchive(t, db, root, backup.id, backup.trigger, backup.consistent, backup.at)
		queued, err := db.QueueRemoteCopy(t.Context(), "inst-a", backup.id, nil)
		if err != nil {
			t.Fatalf("queue %s: %v", backup.id, err)
		}
		status := "succeeded"
		if backup.id == "failed" {
			status = "failed"
		}
		seed(t, db, `UPDATE remote_copies SET status=?,archive_created_at=? WHERE id=?`,
			status, store.FormatTime(backup.at), queued.ID)
	}
	if err := rt.RemoteBackups().markRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "cold-old", want: true},
		{id: "cold-new", want: false},
		{id: "hot-old", want: true},
		{id: "hot-new", want: false},
		{id: "snapshot-old", want: true},
		{id: "snapshot-new", want: false},
		{id: "failed", want: false},
	} {
		var got bool
		if err := db.Reader.QueryRowContext(t.Context(),
			`SELECT cleanup_pending FROM remote_copies WHERE backup_id=?`, tc.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("copy %s cleanup_pending = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestRemoteRetentionRefusesToDeleteUnrelatedObjects(t *testing.T) {
	rt, db, root, _, _ := backupsWorld(t)
	saveRemoteDestination(t, db)
	seedArchive(t, db, root, "b-unrelated", store.TriggerManual, true, time.Now().UTC())
	remoteCopy, err := db.QueueRemoteCopy(t.Context(), "inst-a", "b-unrelated", nil)
	if err != nil {
		t.Fatal(err)
	}
	object, err := json.Marshal(remote.ObjectRef{Key: "other-service/valuable.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(remote.ObjectRef{Key: "other-service/valuable.tar.gz.json"})
	if err != nil {
		t.Fatal(err)
	}
	remoteCopy.ObjectJSON, remoteCopy.ManifestJSON = string(object), string(manifest)
	fake := &remoteBackupBackend{objects: make(map[string][]byte)}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) { return fake, nil }

	err = rt.RemoteBackups().deleteRemoteObjects(t.Context(), remoteCopy)
	if !errors.Is(err, remote.ErrConfiguration) {
		t.Fatalf("delete unrelated remote keys = %v, want ErrConfiguration", err)
	}
	fake.mu.Lock()
	deletes := fake.deletes
	fake.mu.Unlock()
	if deletes != 0 {
		t.Errorf("delete calls = %d, want zero for unrelated remote objects", deletes)
	}
}

type remoteBackupBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	stats   int
	deletes int
	putErr  error
	entered chan struct{}
	block   bool
}

func (b *remoteBackupBackend) Put(ctx context.Context, key, localPath string) (remote.Object, error) {
	if err := ctx.Err(); err != nil {
		return remote.Object{}, err
	}
	b.mu.Lock()
	b.puts++
	putErr, entered, block := b.putErr, b.entered, b.block
	b.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block {
		<-ctx.Done()
		return remote.Object{}, ctx.Err()
	}
	if putErr != nil {
		return remote.Object{}, putErr
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return remote.Object{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
	return remote.Object{Ref: remote.ObjectRef{Key: key}, SizeBytes: int64(len(data))}, nil
}

func (b *remoteBackupBackend) Stat(ctx context.Context, ref remote.ObjectRef) (remote.Object, error) {
	if err := ctx.Err(); err != nil {
		return remote.Object{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stats++
	data, ok := b.objects[ref.Key]
	if !ok {
		return remote.Object{}, fmt.Errorf("stat %s: %w", ref.Key, remote.ErrNotFound)
	}
	return remote.Object{Ref: ref, SizeBytes: int64(len(data))}, nil
}

func (b *remoteBackupBackend) Delete(ctx context.Context, ref remote.ObjectRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deletes++
	delete(b.objects, ref.Key)
	return nil
}

func queueCopyForWorker(t *testing.T, db *store.DB, root, backupID string) *store.RemoteCopy {
	t.Helper()
	saveRemoteDestination(t, db)
	path := seedArchive(t, db, root, backupID, store.TriggerManual, true, time.Now().UTC())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	seed(t, db, `UPDATE backups SET size_bytes=?,sha256=? WHERE id=?`,
		len(data), hex.EncodeToString(digest[:]), backupID)
	remoteCopy, err := db.QueueRemoteCopy(t.Context(), "inst-a", backupID, nil)
	if err != nil {
		t.Fatalf("QueueRemoteCopy: %v", err)
	}
	return remoteCopy
}

func waitRemoteCopy(t *testing.T, db *store.DB, c *store.RemoteCopy) *store.RemoteCopy {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := db.RemoteCopyByID(t.Context(), c.InstanceID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "pending" && got.Status != "uploading" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("remote copy %s did not finish, last status = %s", c.ID, c.Status)
	return nil
}

func TestRemoteCopyWorkerUploadsArchiveAndManifest(t *testing.T) {
	rt, db, root, _, _ := backupsWorld(t)
	remoteCopy := queueCopyForWorker(t, db, root, "worker-success")
	fake := &remoteBackupBackend{objects: make(map[string][]byte)}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) {
		return fake, nil
	}

	rt.RemoteBackups().dispatchRemote(t.Context())
	got := waitRemoteCopy(t, db, remoteCopy)
	if got.Status != "succeeded" || got.Attempts != 1 || got.SucceededAt == nil {
		t.Fatalf("worker result = %+v, want one successful attempt", got)
	}
	var archiveRef, manifestRef remote.ObjectRef
	if err := json.Unmarshal([]byte(got.ObjectJSON), &archiveRef); err != nil {
		t.Fatalf("decode archive reference %q: %v", got.ObjectJSON, err)
	}
	if err := json.Unmarshal([]byte(got.ManifestJSON), &manifestRef); err != nil {
		t.Fatalf("decode manifest reference %q: %v", got.ManifestJSON, err)
	}
	wantKey := "valmin/remote-destination/inst-a/worker-success.tar.gz"
	if archiveRef.Key != wantKey || manifestRef.Key != wantKey+".json" {
		t.Fatalf("recorded object keys = %q and %q, want archive and manifest", archiveRef.Key, manifestRef.Key)
	}
	fake.mu.Lock()
	archive, archiveOK := fake.objects[archiveRef.Key]
	manifest, manifestOK := fake.objects[manifestRef.Key]
	puts, stats := fake.puts, fake.stats
	fake.mu.Unlock()
	if !archiveOK || !manifestOK || puts != 2 || stats != 2 {
		t.Fatalf("backend objects archive:%v manifest:%v, put:%d stat:%d; want both objects, 2 puts, 2 stats",
			archiveOK, manifestOK, puts, stats)
	}
	if len(archive) != int(got.SizeBytes) {
		t.Errorf("uploaded archive size = %d, want %d", len(archive), got.SizeBytes)
	}
	var manifestBody struct {
		Version  int    `json:"version"`
		BackupID string `json:"backup_id"`
		SHA256   string `json:"sha256"`
		Size     int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(manifest, &manifestBody); err != nil {
		t.Fatalf("decode uploaded manifest: %v", err)
	}
	if manifestBody.Version != 1 || manifestBody.BackupID != remoteCopy.BackupID ||
		manifestBody.SHA256 != remoteCopy.SHA256 || manifestBody.Size != remoteCopy.SizeBytes {
		t.Errorf("uploaded manifest = %+v, want catalogue identity and checksum", manifestBody)
	}
}

func TestRemoteCopyWorkerClassifiesTransferFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *remote.Failure
		status  string
	}{
		{name: "transient", failure: &remote.Failure{Message: "temporary outage", Temporary: true}, status: "retry_wait"},
		{name: "permanent", failure: &remote.Failure{Message: "permission denied"}, status: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, root, _, _ := backupsWorld(t)
			remoteCopy := queueCopyForWorker(t, db, root, "worker-"+tc.name)
			fake := &remoteBackupBackend{objects: make(map[string][]byte), putErr: tc.failure}
			rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) {
				return fake, nil
			}

			rt.RemoteBackups().dispatchRemote(t.Context())
			got := waitRemoteCopy(t, db, remoteCopy)
			if got.Status != tc.status || got.Attempts != 1 || got.LastError == "" {
				t.Errorf("worker failure result = %+v, want %s after one attempt", got, tc.status)
			}
		})
	}
}

func TestRemoteCopyWorkerCancellationStopsBackend(t *testing.T) {
	rt, db, root, _, _ := backupsWorld(t)
	remoteCopy := queueCopyForWorker(t, db, root, "worker-cancel")
	fake := &remoteBackupBackend{
		objects: make(map[string][]byte), entered: make(chan struct{}, 1), block: true,
	}
	rt.RemoteBackups().BackendFor = func(*store.RemoteDestination) (remote.Backend, error) {
		return fake, nil
	}
	rt.RemoteBackups().dispatchRemote(t.Context())
	select {
	case <-fake.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start remote transfer")
	}
	if err := db.CancelRemoteCopy(t.Context(), remoteCopy.InstanceID, remoteCopy.ID, nil); err != nil {
		t.Fatalf("CancelRemoteCopy: %v", err)
	}
	got := waitRemoteCopy(t, db, remoteCopy)
	if got.Status != "cancelled" || !got.CancelRequested {
		t.Fatalf("worker cancellation result = %+v, want cancelled", got)
	}
}

func TestRemoteBackoffStaysWithinJitterAndHourlyCap(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		min     time.Duration
		max     time.Duration
	}{
		{attempt: 1, min: time.Minute, max: 72 * time.Second},
		{attempt: 2, min: 2 * time.Minute, max: 144 * time.Second},
		{attempt: 20, min: time.Hour, max: time.Hour},
	} {
		for range 25 {
			got := remoteBackoff(tc.attempt)
			if got < tc.min || got > tc.max {
				t.Errorf("remoteBackoff(%d) = %s, want within [%s, %s]", tc.attempt, got, tc.min, tc.max)
			}
		}
	}
}
