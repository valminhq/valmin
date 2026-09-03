package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "panel.db")
	db, err := store.Open(t.Context(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func testConfig() Config {
	return Config{
		LeaseTTL:         60 * time.Millisecond,
		ProgressInterval: time.Millisecond,
		LogCap:           64 << 10,
		RetentionDays:    30,
	}
}

func noop(_ context.Context, _ *Handle) Outcome { return Outcome{Status: "succeeded"} }

// TestSubmitRejectsSecondHolder asserts that two concurrent submissions on the same lock
// produce one job row and one conflict carrying its id (ADR-030).
func TestSubmitRejectsSecondHolder(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	block := make(chan struct{})
	started := make(chan struct{}, 2)

	blocking := func(_ context.Context, _ *Handle) Outcome {
		started <- struct{}{}
		<-block
		return Outcome{Status: "succeeded"}
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	jobs := make([]*store.Job, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:race"}, blocking)
			jobs[i], results[i] = j, err
		}(i)
	}
	wg.Wait()
	close(block)

	var successes, conflicts int
	var conflict *store.JobConflict
	for i := range 2 {
		switch {
		case results[i] == nil:
			successes++
		case errors.As(results[i], &conflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", results[i])
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 and 1", successes, conflicts)
	}

	var winner *store.Job
	for _, j := range jobs {
		if j != nil {
			winner = j
		}
	}
	if conflict.JobID != winner.ID || conflict.Kind != "start" {
		t.Errorf("conflict = %+v, want job %s (start)", conflict, winner.ID)
	}
	<-started // the winner's runner always sends before blocking on release
}

// TestWorkRunsOutsideAnyTransaction is the instrumented check that C1 holds: nothing calls
// Docker, the filesystem or the network between BEGIN and COMMIT on the
// writer pool. Proven structurally here: while the Runner is still running (after Submit
// returned, so the Claim transaction has already committed), a completely unrelated write
// against the same single-connection writer pool must not block — if Claim's transaction
// were still open, this UPDATE would deadlock against the same connection.
func TestWorkRunsOutsideAnyTransaction(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())

	inWork := make(chan struct{})
	release := make(chan struct{})
	runner := func(ctx context.Context, h *Handle) Outcome {
		close(inWork)
		<-release
		return Outcome{Status: "succeeded"}
	}

	_, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:txn"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	<-inWork

	done := make(chan error, 1)
	go func() {
		_, err := db.Writer.ExecContext(t.Context(), `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)`,
			"probe", "1", store.Now())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent write while work runs: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent write blocked — the claim transaction is still open during work")
	}
	close(release)
}

// TestLeaseLossAbandonsWithoutTerminalStatus is C17: a worker whose lease_owner changes out
// from under it stops and writes no terminal status.
func TestLeaseLossAbandonsWithoutTerminalStatus(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())

	var sawCancel atomic.Bool
	started := make(chan struct{})
	runner := func(ctx context.Context, h *Handle) Outcome {
		close(started)
		<-ctx.Done()
		sawCancel.Store(true)
		return Outcome{Status: "succeeded"} // must never reach the database
	}

	j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:lease"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	<-started

	// Simulate the crash-recovery sweep taking the row over under a different boot.
	if _, err := db.Writer.ExecContext(t.Context(),
		`UPDATE job_runs SET lease_owner = ? WHERE id = ?`, "panel:boot-b", j.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for !sawCancel.Load() {
		select {
		case <-deadline:
			t.Fatal("runner never observed lease loss")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Give run() a moment to reach its post-runner check, then assert nothing was written.
	time.Sleep(50 * time.Millisecond)
	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running (no terminal status written)", got.Status)
	}
}

// TestLogCappedAtFinish asserts that a job producing far more than jobs.log_cap of output
// ends with exactly one, bounded log value.
func TestLogCappedAtFinish(t *testing.T) {
	db := testDB(t)
	cfg := testConfig()
	cfg.LogCap = 1024
	e := New(db, "panel:boot-a", cfg)

	line := strings.Repeat("x", 100)
	runner := func(ctx context.Context, h *Handle) Outcome {
		for range 200 { // 200 * ~101 bytes ~= 20 KiB, far past the 1 KiB cap
			h.Log(line)
		}
		return Outcome{Status: "succeeded"}
	}

	j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:log"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, db, j.ID)

	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Log == nil {
		t.Fatal("log not persisted")
	}
	if len(*got.Log) > cfg.LogCap {
		t.Errorf("log length = %d, want <= %d", len(*got.Log), cfg.LogCap)
	}
	if !strings.Contains(*got.Log, "truncated") {
		t.Error("log does not carry the truncation marker")
	}
}

// TestCancelPastPointOfNoReturn asserts a cancel is refused past the point of no return.
func TestCancelPastPointOfNoReturn(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())
	e.RegisterCancelPolicy(KindProvision, func(checkpoint string) (bool, string) {
		return checkpoint == "", "cloned" // only cancellable before the first checkpoint
	})

	block := make(chan struct{})
	runner := func(ctx context.Context, h *Handle) Outcome {
		<-block
		return Outcome{Status: "succeeded"}
	}
	j, err := e.Submit(t.Context(), &Spec{Kind: KindProvision, LockKey: "instance:cancel"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer close(block)

	if _, err := db.Writer.ExecContext(t.Context(),
		`UPDATE job_runs SET checkpoint = ? WHERE id = ?`, "cloned", j.ID); err != nil {
		t.Fatal(err)
	}

	err = e.Cancel(t.Context(), j.ID)
	var notCancellable *ErrNotCancellable
	if !errors.As(err, &notCancellable) {
		t.Fatalf("cancel past checkpoint: got %v, want *ErrNotCancellable", err)
	}
	if notCancellable.Phase != "cloned" {
		t.Errorf("phase = %q, want cloned", notCancellable.Phase)
	}
}

// TestCancelBeforePointOfNoReturn is the same policy's other branch.
func TestCancelBeforePointOfNoReturn(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())
	e.RegisterCancelPolicy(KindProvision, func(checkpoint string) (bool, string) {
		return checkpoint == "", "cloned"
	})

	block := make(chan struct{})
	runner := func(ctx context.Context, h *Handle) Outcome {
		<-block
		return Outcome{Status: "cancelled"}
	}
	j, err := e.Submit(t.Context(), &Spec{Kind: KindProvision, LockKey: "instance:cancel2"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer close(block)

	if err := e.Cancel(t.Context(), j.ID); err != nil {
		t.Fatalf("cancel before checkpoint: %v", err)
	}
	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CancelRequestedAt == nil {
		t.Error("cancel_requested_at not set")
	}
}

// TestSubmitAndRunSucceeds is the straightforward path, exercised once end to end so the
// happy path is not only proven by teardown of the harder tests above.
func TestSubmitAndRunSucceeds(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())

	j, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:ok"}, noop)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, db, j.ID)

	got, err := db.JobByID(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", got.Status)
	}

	// The lock must be released: a second submission on the same key now succeeds.
	if _, err := e.Submit(t.Context(), &Spec{Kind: KindStart, LockKey: "instance:ok"}, noop); err != nil {
		t.Fatalf("re-submit after finish: %v", err)
	}
}

func waitForTerminal(t *testing.T, db *store.DB, jobID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		j, err := db.JobByID(t.Context(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j != nil && j.Status != "running" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("job never reached a terminal status")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestStateIsAnnouncedAfterEachTransactionCommits is 14 §4.4 from the engine's side: the
// panel's live state topic is fed from exactly two moments here, and both are after a
// commit.
//
// It also pins where those moments are. Announcing from inside OnClaim or OnFinish
// would announce a transition that can still roll back, and on a lossless topic the client
// gets no correction it can distinguish from the original.
func TestStateIsAnnouncedAfterEachTransactionCommits(t *testing.T) {
	db := testDB(t)
	e := New(db, "panel:boot-a", testConfig())

	var mu sync.Mutex
	var announced []string
	e.Announce(func(_ context.Context, instanceID string) {
		mu.Lock()
		defer mu.Unlock()
		// The row has to be readable as its new state by the time this runs, or the
		// publisher would announce what the transaction was about to undo.
		announced = append(announced, instanceID)
	})

	instanceID := "inst-a"
	if _, err := db.Writer.ExecContext(context.Background(), `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES (?, 'a', 'stopped', '/srv/a', 2456, 'A', 'W', 'v1.k.n.c', 'cp-a', ?, ?)`,
		instanceID, store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	_, err := e.Submit(context.Background(), &Spec{
		Kind: KindStart, LockKey: "instance:" + instanceID,
		InstanceID: &instanceID, InstanceName: "a",
	}, func(context.Context, *Handle) Outcome {
		mu.Lock()
		atClaim := len(announced)
		mu.Unlock()
		if atClaim != 1 {
			t.Errorf("the claim announced %d times, want 1", atClaim)
		}
		close(done)
		return Outcome{Status: "succeeded"}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(announced)
		mu.Unlock()
		if n == 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("announcements = %v, want one at claim and one at finish", announced)
}

// TestAGlobalJobAnnouncesNothing: a job with no instance has no instance state to publish,
// and calling the publisher with an empty id would make it read a row that cannot exist.
func TestAGlobalJobAnnouncesNothing(t *testing.T) {
	e := New(testDB(t), "panel:boot-a", testConfig())
	var calls atomic.Int64
	e.Announce(func(context.Context, string) { calls.Add(1) })

	if _, err := e.Submit(context.Background(), &Spec{
		Kind: KindStart, LockKey: "global:test",
	}, noop); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("a global job announced %d instance transitions", got)
	}
}
