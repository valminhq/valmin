package store

import (
	"errors"
	"testing"
	"time"
)

func remoteBackupFixture(t *testing.T) (db *DB, instanceID string) {
	t.Helper()
	db = open(t)
	instanceID = seedInstance(t, db, "remote-instance", 2456)
	exec(t, db.Writer, `INSERT INTO remote_backup_destinations
		(id,kind,enabled,remote_name,folder,created_at,updated_at)
		VALUES ('remote-destination','rclone',?,'archive','valmin',?,?)`, true, Now(), Now())
	return db, instanceID
}

func remoteBackup(t *testing.T, db *DB, instanceID, backupID string) *Backup {
	t.Helper()
	b := &Backup{
		ID: backupID, InstanceID: instanceID, Path: "/archives/" + backupID + ".tar.gz",
		SizeBytes: 128, SHA256: "abc123", WorldName: "World", Trigger: TriggerScheduled, Consistent: true,
	}
	if err := db.CreateBackup(t.Context(), b); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	return b
}

func TestCreateBackupAutomaticallyQueuesRemoteCopy(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	exec(t, db.Writer, `UPDATE instances SET remote_backup_enabled=TRUE WHERE id=?`, instanceID)

	b := remoteBackup(t, db, instanceID, "automatic")
	rows, err := db.ListRemoteCopies(t.Context(), instanceID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("remote copies = %d, want one durable automatic enqueue", len(rows))
	}
	got := rows[0]
	if got.BackupID != b.ID || got.Status != "pending" || got.SourcePath != b.Path || got.SizeBytes != b.SizeBytes ||
		got.SHA256 != b.SHA256 {
		t.Errorf("automatic copy = %+v, want pending copy of the committed backup", got)
	}

	// Automatic copies require both an enabled destination and the per-instance policy.
	exec(t, db.Writer, `UPDATE instances SET remote_backup_enabled=FALSE WHERE id=?`, instanceID)
	if err := db.CreateBackup(t.Context(), &Backup{
		ID: "policy-off", InstanceID: instanceID, Path: "/archives/policy-off.tar.gz",
		WorldName: "World", Trigger: TriggerScheduled, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	exec(t, db.Writer, `UPDATE instances SET remote_backup_enabled=TRUE WHERE id=?`, instanceID)
	exec(t, db.Writer, `UPDATE remote_backup_destinations SET enabled=FALSE WHERE id='remote-destination'`)
	if err := db.CreateBackup(t.Context(), &Backup{
		ID: "destination-off", InstanceID: instanceID, Path: "/archives/destination-off.tar.gz",
		WorldName: "World", Trigger: TriggerScheduled, Consistent: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = db.ListRemoteCopies(t.Context(), instanceID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("copies after ineligible backups = %d, want only the eligible backup", len(rows))
	}
}

func TestSafetySnapshotAutomaticallyQueuesRemoteCopy(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	exec(t, db.Writer, `UPDATE instances SET remote_backup_enabled=TRUE WHERE id=?`, instanceID)

	b := &Backup{
		ID: "safety-snapshot", InstanceID: instanceID, Path: "/archives/safety-snapshot.tar.gz",
		SizeBytes: 128, SHA256: "abc123", WorldName: "World", Trigger: TriggerPreRestore, Consistent: true,
	}
	if err := db.CreateBackup(t.Context(), b); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	copies, err := db.ListRemoteCopies(t.Context(), instanceID, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].BackupID != b.ID || copies[0].Status != "pending" {
		t.Fatalf("safety snapshot copies = %+v, want one pending remote copy", copies)
	}
}

func TestBackupTransactionRollbackLeavesNoRemoteIntent(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	exec(t, db.Writer, `UPDATE instances SET remote_backup_enabled=TRUE WHERE id=?`, instanceID)
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Backup{
		ID: "rolled-back", InstanceID: instanceID, Path: "/archives/rolled-back.tar.gz",
		SizeBytes: 128, SHA256: "abc123", WorldName: "World", Trigger: TriggerScheduled, Consistent: true,
	}
	if err := TxCreateBackup(t.Context(), tx, b); err != nil {
		_ = tx.Rollback()
		t.Fatalf("TxCreateBackup: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var backups, copies int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM backups WHERE id=?`, b.ID).
		Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_copies WHERE backup_id=?`, b.ID).
		Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if backups != 0 || copies != 0 {
		t.Errorf("after transaction rollback: backups=%d copies=%d, want neither", backups, copies)
	}
}

func TestQueueRemoteCopyDeduplicatesBackup(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "deduplicated")
	first, err := db.QueueRemoteCopy(t.Context(), instanceID, "deduplicated", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.QueueRemoteCopy(t.Context(), instanceID, "deduplicated", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.DestinationID != second.DestinationID {
		t.Fatalf("duplicate enqueue returned %s/%s and %s/%s, want same durable row",
			first.ID, first.DestinationID, second.ID, second.DestinationID)
	}
	var count int
	if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_copies WHERE backup_id=?`,
		"deduplicated").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("remote rows for deduplicated backup = %d, want one", count)
	}
}

func TestRemoteSummaryUsesMostRecentSuccessfulTransferArchive(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "recent-transfer-old-archive")
	remoteBackup(t, db, instanceID, "earlier-transfer-new-archive")
	oldArchive, err := db.QueueRemoteCopy(t.Context(), instanceID, "recent-transfer-old-archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	newArchive, err := db.QueueRemoteCopy(t.Context(), instanceID, "earlier-transfer-new-archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	recentTransfer := time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC)
	earlierTransfer := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	oldArchiveAt := time.Date(2025, time.December, 1, 0, 0, 0, 0, time.UTC)
	newArchiveAt := time.Date(2025, time.December, 2, 0, 0, 0, 0, time.UTC)
	exec(t, db.Writer, `UPDATE remote_copies SET status='succeeded',succeeded_at=?,archive_created_at=? WHERE id=?`,
		FormatTime(recentTransfer), FormatTime(oldArchiveAt), oldArchive.ID)
	exec(t, db.Writer, `UPDATE remote_copies SET status='succeeded',succeeded_at=?,archive_created_at=? WHERE id=?`,
		FormatTime(earlierTransfer), FormatTime(newArchiveAt), newArchive.ID)

	summary, err := db.RemoteSummary(t.Context(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.LastSuccessAt == nil || *summary.LastSuccessAt != FormatTime(recentTransfer) ||
		summary.LastArchiveAt == nil || *summary.LastArchiveAt != FormatTime(oldArchiveAt) {
		t.Errorf("remote summary success=%v archive=%v, want newest transfer time and its older archive time",
			summary.LastSuccessAt, summary.LastArchiveAt)
	}
}

func TestReplacingRemoteDestinationCancelsPendingAndKeepsSuccessRefs(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "old-success")
	remoteBackup(t, db, instanceID, "old-pending")
	succeeded, err := db.QueueRemoteCopy(t.Context(), instanceID, "old-success", nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.QueueRemoteCopy(t.Context(), instanceID, "old-pending", nil)
	if err != nil {
		t.Fatal(err)
	}
	refs := `{"key":"valmin/old-account/old-success.tar.gz"}`
	exec(
		t,
		db.Writer,
		`UPDATE remote_copies SET status='succeeded',cleanup_pending=TRUE,object_json=?,manifest_json='{"key":"manifest"}',succeeded_at=? WHERE id=?`,
		refs,
		Now(),
		succeeded.ID,
	)

	if err := db.SaveRemoteDestination(t.Context(), &RemoteDestination{
		ID: "replacement-destination", Kind: "rclone", Enabled: true, RemoteName: "new-archive", Folder: "valmin",
	}, nil); err != nil {
		t.Fatalf("replace destination: %v", err)
	}
	oldPending, err := db.RemoteCopyByID(t.Context(), instanceID, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldSuccess, err := db.RemoteCopyByID(t.Context(), instanceID, succeeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldPending.Status != "cancelled" {
		t.Errorf("pending copy after replacement = %s, want cancelled", oldPending.Status)
	}
	if oldSuccess.Status != "succeeded" || oldSuccess.ObjectJSON != refs ||
		oldSuccess.ManifestJSON != `{"key":"manifest"}` {
		t.Errorf("successful old-account record after replacement = %+v, want preserved object references", oldSuccess)
	}
	oldDestination, err := db.RemoteDestinationByID(t.Context(), "remote-destination")
	if err != nil {
		t.Fatal(err)
	}
	if oldDestination == nil || !oldDestination.Retired || oldDestination.Enabled {
		t.Errorf("replaced destination = %+v, want retired and disabled", oldDestination)
	}
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = TxCheckRemoteCleanup(t.Context(), tx, succeeded.ID)
	_ = tx.Rollback()
	if !errors.Is(err, ErrRemoteUnavailable) {
		t.Errorf("cleanup check for retired destination = %v, want ErrRemoteUnavailable", err)
	}
}

func TestDeletingInstanceKeepsRemoteHistoryAndExcludesCleanup(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "deleted-instance-copy")
	remoteCopy, err := db.QueueRemoteCopy(t.Context(), instanceID, "deleted-instance-copy", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db.Writer, `UPDATE remote_copies SET status='succeeded',cleanup_pending=TRUE WHERE id=?`, remoteCopy.ID)
	exec(t, db.Writer, `UPDATE instances SET state='deleting' WHERE id=?`, instanceID)
	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := TxDeleteInstance(t.Context(), tx, instanceID, "deleting"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("TxDeleteInstance: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := db.RemoteCopyByID(t.Context(), instanceID, remoteCopy.ID)
	if err != nil || got == nil || got.Status != "succeeded" || !got.CleanupPending {
		t.Fatalf("remote history after instance deletion = %+v, %v, want retained", got, err)
	}
	due, err := db.DueRemoteCleanup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("cleanup work for deleted instance = %+v, want excluded", due)
	}
	tx, err = db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = TxCheckRemoteCleanup(t.Context(), tx, remoteCopy.ID)
	_ = tx.Rollback()
	if !errors.Is(err, ErrRemoteUnavailable) {
		t.Errorf("cleanup check for deleted instance = %v, want ErrRemoteUnavailable", err)
	}
}

func TestCredentialRotationClearsDestinationTestResult(t *testing.T) {
	db, _ := remoteBackupFixture(t)
	if err := db.SaveRemoteDestination(t.Context(), &RemoteDestination{
		ID:          "remote-destination",
		Kind:        "rclone",
		Enabled:     true,
		Credentials: "old-secret",
		RemoteName:  "archive",
		Folder:      "valmin",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRemoteTest(t.Context(), "remote-destination", "old credential failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRemoteDestination(t.Context(), &RemoteDestination{
		ID:          "remote-destination",
		Kind:        "rclone",
		Enabled:     true,
		Credentials: "new-secret",
		RemoteName:  "archive",
		Folder:      "valmin",
	}, nil); err != nil {
		t.Fatal(err)
	}
	destination, err := db.RemoteDestination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if destination.Credentials != "new-secret" || destination.LastTestAt != nil || destination.LastTestError != "" {
		t.Errorf("destination after credential rotation = %+v, want cleared connection result", destination)
	}
}

func TestRemoteCopyProtectsItsLocalSourceUntilTerminal(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "protected")
	remoteCopy, err := db.QueueRemoteCopy(t.Context(), instanceID, "protected", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteBackup(t.Context(), instanceID, "protected"); !errors.Is(err, ErrBackupProtected) {
		t.Fatalf("delete during pending upload = %v, want ErrBackupProtected", err)
	}
	exec(t, db.Writer, `UPDATE remote_copies SET status='uploading' WHERE id=?`, remoteCopy.ID)
	if err := db.DeleteBackup(t.Context(), instanceID, "protected"); !errors.Is(err, ErrBackupProtected) {
		t.Fatalf("delete during active upload = %v, want ErrBackupProtected", err)
	}
	exec(t, db.Writer, `UPDATE remote_copies SET status='succeeded' WHERE id=?`, remoteCopy.ID)
	if err := db.DeleteBackup(t.Context(), instanceID, "protected"); err != nil {
		t.Fatalf("delete after successful upload: %v", err)
	}
}

func TestRemoteCopyRetryCancelExpiryAndRecovery(t *testing.T) {
	db, instanceID := remoteBackupFixture(t)
	remoteBackup(t, db, instanceID, "lifecycle")
	remoteCopy, err := db.QueueRemoteCopy(t.Context(), instanceID, "lifecycle", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.CancelRemoteCopy(t.Context(), instanceID, remoteCopy.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := db.RemoteCopyByID(t.Context(), instanceID, remoteCopy.ID)
	if err != nil || got.Status != "cancelled" || !got.CancelRequested {
		t.Fatalf("cancelled copy = %+v, %v", got, err)
	}
	if err := db.RetryRemoteCopy(t.Context(), instanceID, remoteCopy.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = db.RemoteCopyByID(t.Context(), instanceID, remoteCopy.ID)
	if err != nil || got.Status != "pending" || got.CancelRequested || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("retried copy = %+v, %v", got, err)
	}

	// A worker that disappeared while uploading becomes retryable, while expired work fails.
	exec(t, db.Writer, `UPDATE remote_copies SET status='uploading',deadline_at=? WHERE id=?`,
		FormatTime(time.Now().Add(time.Hour)), remoteCopy.ID)
	if err := db.ReconcileRemoteCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = db.RemoteCopyByID(t.Context(), instanceID, remoteCopy.ID)
	if err != nil || got.Status != "retry_wait" || got.LastError != "Previous transfer was interrupted." {
		t.Fatalf("recovered copy = %+v, %v", got, err)
	}
	exec(t, db.Writer, `UPDATE remote_copies SET status='retry_wait',deadline_at=? WHERE id=?`,
		FormatTime(time.Now().Add(-time.Second)), remoteCopy.ID)
	if err := db.ReconcileRemoteCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err = db.RemoteCopyByID(t.Context(), instanceID, remoteCopy.ID)
	if err != nil || got.Status != "failed" ||
		got.LastError != "Upload retry window expired or source archive is unavailable." {
		t.Fatalf("expired copy = %+v, %v", got, err)
	}
}
