package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func newJob(id, kind, lockKey string) *Job {
	return &Job{ID: id, Kind: kind, LockKey: lockKey, InstanceName: "test"}
}

// TestClaimJobRejectsSecondHolder is ADR-030: reject, don't queue. A second claim on the
// same lock_key gets *JobConflict naming the first job's id and kind, never a second row.
func TestClaimJobRejectsSecondHolder(t *testing.T) {
	db := open(t)
	owner := "panel:boot-a"
	until := time.Now().Add(30 * time.Second)

	first := newJob(NewID(), "start", "instance:abc")
	if err := db.ClaimJob(t.Context(), first, owner, until, nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	second := newJob(NewID(), "stop", "instance:abc")
	err := db.ClaimJob(t.Context(), second, owner, until, nil)
	var conflict *JobConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("second claim: got %v, want *JobConflict", err)
	}
	if conflict.JobID != first.ID || conflict.Kind != "start" {
		t.Errorf("conflict = %+v, want job %s (start)", conflict, first.ID)
	}

	var count int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE lock_key = ?`, "instance:abc").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("job_runs rows for lock = %d, want 1", count)
	}
}

// TestClaimJobOnClaimRunsInTheSameTransaction proves the seam a state flip needs: a write
// made by onClaim commits atomically with the lock and job row, and rolls back with them if
// onClaim fails — a side effect like an instance's transient state must never land only
// half done.
func TestClaimJobOnClaimRunsInTheSameTransaction(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, NewID(), 2456)

	j := newJob(NewID(), "provision", "instance:"+instanceID)
	iid := instanceID
	j.InstanceID = &iid
	err := db.ClaimJob(t.Context(), j, "panel:boot-a", time.Now().Add(30*time.Second),
		func(_ context.Context, tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE instances SET state = 'provisioning' WHERE id = ?`, instanceID)
			return err
		})
	if err != nil {
		t.Fatalf("claim with onClaim: %v", err)
	}

	var state string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT state FROM instances WHERE id = ?`, instanceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "provisioning" {
		t.Errorf("instance state = %q, want provisioning", state)
	}

	// A failing onClaim must roll back the lock and the job row too — the state flip is
	// not the only thing that must not land half-committed.
	j2 := newJob(NewID(), "provision", "instance:"+NewID())
	failErr := errors.New("boom")
	err = db.ClaimJob(t.Context(), j2, "panel:boot-a", time.Now().Add(30*time.Second),
		func(_ context.Context, _ *sql.Tx) error { return failErr })
	if !errors.Is(err, failErr) {
		t.Fatalf("claim with failing onClaim: got %v, want %v", err, failErr)
	}
	if got, err := db.JobByID(t.Context(), j2.ID); err != nil || got != nil {
		t.Errorf("job row survived a rolled-back claim: %+v, err %v", got, err)
	}
}

// TestRenewJobLeaseFailsWhenOwnerChanged is C17: losing the lease is fatal to the job.
func TestRenewJobLeaseFailsWhenOwnerChanged(t *testing.T) {
	db := open(t)
	j := newJob(NewID(), "start", "instance:xyz")
	if err := db.ClaimJob(t.Context(), j, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}

	ok, err := db.RenewJobLease(t.Context(), j.ID, "panel:boot-a", time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("renew by the owning boot should succeed")
	}

	// Simulate a crash-recovery sweep taking the job over under a new boot id.
	exec(t, db.Writer, `UPDATE job_runs SET lease_owner = ? WHERE id = ?`, "panel:boot-b", j.ID)

	ok, err = db.RenewJobLease(t.Context(), j.ID, "panel:boot-a", time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("renew by the old boot should fail once lease_owner changed")
	}
}

// TestFinishJobReleasesLockAndWritesTerminalStatus covers 12 §6's Finish phase.
func TestFinishJobReleasesLockAndWritesTerminalStatus(t *testing.T) {
	db := open(t)
	j := newJob(NewID(), "start", "instance:finish")
	if err := db.ClaimJob(t.Context(), j, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}

	logTail := "line one\nline two"
	if err := db.FinishJob(t.Context(), j.ID, "succeeded", 100, nil, nil, &logTail, nil, time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.Progress != 100 {
		t.Errorf("job = %+v, want succeeded/100", got)
	}
	if got.Log == nil || *got.Log != logTail {
		t.Errorf("log = %v, want %q", got.Log, logTail)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease_owner = %v, want cleared", *got.LeaseOwner)
	}

	// The lock must be gone: a fresh claim on the same key must now succeed.
	again := newJob(NewID(), "start", "instance:finish")
	if err := db.ClaimJob(t.Context(), again, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatalf("re-claim after finish: %v", err)
	}
}

// TestUpdateJobCheckpointWrites is 12 §9.4's resume marker: an unthrottled, single-
// statement write, unlike progress.
func TestUpdateJobCheckpointWrites(t *testing.T) {
	db := open(t)
	j := newJob(NewID(), "provision", "instance:checkpoint")
	if err := db.ClaimJob(t.Context(), j, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateJobCheckpoint(t.Context(), j.ID, "dirs_created"); err != nil {
		t.Fatal(err)
	}
	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Checkpoint == nil || *got.Checkpoint != "dirs_created" {
		t.Errorf("checkpoint = %v, want dirs_created", got.Checkpoint)
	}

	if err := db.UpdateJobCheckpoint(t.Context(), j.ID, "build_cached"); err != nil {
		t.Fatal(err)
	}
	got, err = db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Checkpoint == nil || *got.Checkpoint != "build_cached" {
		t.Errorf("checkpoint = %v, want build_cached (overwritten, not appended)", got.Checkpoint)
	}
}

// TestRequestJobCancelIsIdempotent covers 12 §8's cancel_requested_at write.
func TestRequestJobCancelIsIdempotent(t *testing.T) {
	db := open(t)
	j := newJob(NewID(), "provision", "instance:cancel")
	if err := db.ClaimJob(t.Context(), j, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := db.RequestJobCancel(t.Context(), j.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CancelRequestedAt == nil {
		t.Fatal("cancel_requested_at not set")
	}
	first := *got.CancelRequestedAt

	// A second request must not move the timestamp.
	if err := db.RequestJobCancel(t.Context(), j.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CancelRequestedAt.Equal(first) {
		t.Errorf("cancel_requested_at moved: %v -> %v", first, *got.CancelRequestedAt)
	}
}

// TestStaleJobsFindsOnlyAnotherBootsRunningRows is 12 §9.1 step 2's whole question: a
// `running` row naming any owner but this boot's belongs to a process that no longer exists.
func TestStaleJobsFindsOnlyAnotherBootsRunningRows(t *testing.T) {
	db := open(t)
	me, dead := "panel:boot-b", "panel:boot-a"

	mine := newJob(NewID(), "start", "instance:mine")
	if err := db.ClaimJob(t.Context(), mine, me, time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	theirs := newJob(NewID(), "provision", "instance:theirs")
	if err := db.ClaimJob(t.Context(), theirs, dead, time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	// A terminal row from the dead boot is history, not work to sweep.
	finished := newJob(NewID(), "stop", "instance:finished")
	if err := db.ClaimJob(t.Context(), finished, dead, time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(
		t.Context(), finished.ID, "succeeded", 100, nil, nil, nil, nil, time.Now(), nil,
	); err != nil {
		t.Fatal(err)
	}

	stale, err := db.StaleJobs(t.Context(), me)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != theirs.ID {
		t.Fatalf("stale = %+v, want only the dead boot's running job %s", stale, theirs.ID)
	}
}

// TestHeldLockKeysReportsLiveLocksOnly is C14's input: the observer stays silent about any
// instance whose lock is held, and a finished job's lock is not held.
func TestHeldLockKeysReportsLiveLocksOnly(t *testing.T) {
	db := open(t)
	live := newJob(NewID(), "start", "instance:live")
	if err := db.ClaimJob(t.Context(), live, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	done := newJob(NewID(), "start", "instance:done")
	if err := db.ClaimJob(t.Context(), done, "panel:boot-a", time.Now().Add(30*time.Second), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(t.Context(), done.ID, "succeeded", 100, nil, nil, nil, nil, time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	held, err := db.HeldLockKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !held["instance:live"] {
		t.Error("a running job's lock is not reported as held")
	}
	if held["instance:done"] {
		t.Error("a finished job's lock is still reported as held")
	}
}

// TestLastJobForInstanceReturnsTheMostRecent backs 12 §9.2's two rows that need the dead
// job's own detail: provision's checkpoint, and delete's keep_worlds payload.
func TestLastJobForInstanceReturnsTheMostRecent(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, NewID(), 2471)

	now := time.Now()
	var newest string
	for i, kind := range []string{"provision", "start", "stop"} {
		id := NewID()
		newest = id
		exec(t, db.Writer, `
			INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
			VALUES (?, ?, 'succeeded', ?, ?, 'test', '{}', ?)`,
			id, kind, "instance:"+id, instanceID, FormatTime(now.Add(time.Duration(i)*time.Second)))
	}

	got, err := db.LastJobForInstance(t.Context(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != newest || got.Kind != "stop" {
		t.Fatalf("last job = %+v, want %s (stop)", got, newest)
	}

	if got, err := db.LastJobForInstance(t.Context(), NewID()); err != nil || got != nil {
		t.Errorf("an instance with no jobs = %+v, err %v, want (nil, nil)", got, err)
	}
}

// TestSweepTerminalJobsKeepsMostRecent500PerInstance is 12 §7's retention sweep.
func TestSweepTerminalJobsKeepsMostRecent500PerInstance(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, NewID(), 2461)

	now := time.Now()
	for i := 0; i < 600; i++ {
		id := NewID()
		exec(t, db.Writer, `
			INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at, finished_at)
			VALUES (?, 'start', 'succeeded', ?, ?, 'test', '{}', ?, ?)`,
			id, "instance:"+id, instanceID, FormatTime(now.Add(time.Duration(i)*time.Second)), FormatTime(now))
	}

	n, err := db.SweepTerminalJobs(t.Context(), now.Add(time.Hour), 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Errorf("swept %d rows, want 100", n)
	}

	var remaining int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE instance_id = ?`, instanceID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 500 {
		t.Errorf("remaining = %d, want 500", remaining)
	}
}

// TestSweepTerminalJobsRespectsAgeCutoff covers the other half of "whichever bites first".
func TestSweepTerminalJobsRespectsAgeCutoff(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, NewID(), 2466)

	old := NewID()
	exec(t, db.Writer, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at, finished_at)
		VALUES (?, 'start', 'succeeded', ?, ?, 'test', '{}', ?, ?)`,
		old, "instance:"+old, instanceID,
		FormatTime(time.Now().AddDate(0, 0, -31)), FormatTime(time.Now().AddDate(0, 0, -31)))

	recent := NewID()
	exec(t, db.Writer, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at, finished_at)
		VALUES (?, 'start', 'succeeded', ?, ?, 'test', '{}', ?, ?)`,
		recent, "instance:"+recent, instanceID, FormatTime(time.Now()), FormatTime(time.Now()))

	n, err := db.SweepTerminalJobs(t.Context(), time.Now(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if got, err := db.JobByID(t.Context(), old); err != nil || got != nil {
		t.Errorf("old job still present: %+v, err %v", got, err)
	}
	if got, err := db.JobByID(t.Context(), recent); err != nil || got == nil {
		t.Errorf("recent job missing: err %v", err)
	}
}

// TestListJobsForInstancePagesNewestFirst is the keyset walk behind GET
// /instances/{id}/jobs (ADR-099). The page boundary has to be stable while jobs are still
// being written, which is why the cursor carries the id as well as the timestamp — two rows
// created inside the same clock tick are otherwise a coin toss, and the row on the boundary
// is either shown twice or not at all.
func TestListJobsForInstancePagesNewestFirst(t *testing.T) {
	db := open(t)
	mine := seedInstance(t, db, NewID(), 2471)
	theirs := seedInstance(t, db, NewID(), 2476)

	now := time.Now()
	for i := range 5 {
		id := NewID()
		exec(t, db.Writer, `
			INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
			VALUES (?, 'start', 'succeeded', ?, ?, 'test', '{}', ?)`,
			id, "instance:"+id, mine, FormatTime(now.Add(time.Duration(i)*time.Second)))
	}
	other := NewID()
	exec(t, db.Writer, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, payload, created_at)
		VALUES (?, 'start', 'succeeded', ?, ?, 'test', '{}', ?)`,
		other, "instance:"+other, theirs, FormatTime(now))

	first, err := db.ListJobsForInstance(t.Context(), mine, "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("first page = %d rows, want 3", len(first))
	}
	if !first[0].CreatedAt.After(first[2].CreatedAt) {
		t.Errorf("page is not newest first: %v then %v", first[0].CreatedAt, first[2].CreatedAt)
	}

	last := first[len(first)-1]
	second, err := db.ListJobsForInstance(t.Context(), mine, FormatTime(last.CreatedAt), last.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 {
		t.Fatalf("second page = %d rows, want 2", len(second))
	}

	seen := map[string]bool{}
	for _, j := range append(append([]Job{}, first...), second...) {
		if seen[j.ID] {
			t.Errorf("job %s appears on both pages", j.ID)
		}
		seen[j.ID] = true
		if j.InstanceID == nil || *j.InstanceID != mine {
			// D2's whole point: another instance's history must not leak in through a list
			// scoped by an id the caller was authorized against.
			t.Errorf("job %s belongs to another instance", j.ID)
		}
	}
	if len(seen) != 5 {
		t.Errorf("saw %d distinct jobs across both pages, want 5", len(seen))
	}

	if rows, err := db.ListJobsForInstance(t.Context(), NewID(), "", "", 10); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Errorf("an instance with no jobs = %d rows, want 0", len(rows))
	}
}
