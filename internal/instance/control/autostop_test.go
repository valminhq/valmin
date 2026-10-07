package control

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

func ptr(n int) *int { return &n }

// TestIdleClock asserts the clock starts on a known zero, fires once the limit has passed, and
// resets on players, an unknown count, a server that is not running, or auto-stop turned off.
func TestIdleClock(t *testing.T) {
	const limit = 10 * time.Minute
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	type step struct {
		running bool
		players *int
		limit   time.Duration
		at      time.Duration
		want    bool
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"fires once the limit has passed", []step{
			{true, ptr(0), limit, 0, false},
			{true, ptr(0), limit, limit - time.Second, false},
			{true, ptr(0), limit, limit, true},
		}},
		{"a player resets the clock", []step{
			{true, ptr(0), limit, 0, false},
			{true, ptr(1), limit, 5 * time.Minute, false},
			{true, ptr(0), limit, 6 * time.Minute, false},
			{true, ptr(0), limit, 15 * time.Minute, false},
			{true, ptr(0), limit, 16 * time.Minute, true},
		}},
		{"an unknown count resets the clock", []step{
			{true, ptr(0), limit, 0, false},
			{true, nil, limit, 5 * time.Minute, false},
			{true, ptr(0), limit, 11 * time.Minute, false},
		}},
		{"a stopped server resets the clock", []step{
			{true, ptr(0), limit, 0, false},
			{false, ptr(0), limit, 5 * time.Minute, false},
			{true, ptr(0), limit, 11 * time.Minute, false},
		}},
		{"auto-stop off never fires", []step{
			{true, ptr(0), 0, 0, false},
			{true, ptr(0), 0, time.Hour, false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := idleClock{}
			for i, s := range tt.steps {
				if got := c.observe("inst-a", s.running, s.players, s.limit, t0.Add(s.at)); got != s.want {
					t.Fatalf("step %d: observe = %v, want %v", i, got, s.want)
				}
			}
		})
	}
}

// TestAutoStopSubmitsAStopForAnEmptyServer asserts a running server whose log reports no players
// gets one stop job past its limit, recorded as an auto-stop, and that a held lock defers it.
func TestAutoStopSubmitsAStopForAnEmptyServer(t *testing.T) {
	w, db, fake, _ := supervisorWorld(t)
	containerID := seedInstance(t, w, "running")
	seed(t, db, `UPDATE instances SET auto_stop_minutes = 5 WHERE id = ?`, seededInstanceID)
	if err := w.c.Supervisor.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fake.Get(containerID).Stdout("10/07/2026 12:00:00: Connections 0 ZDOS:120  sent:0 recv:0\n")
	deadline := time.Now().Add(2 * time.Second)
	for w.c.Supervisor.players(seededInstanceID) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the reader never learned the player count")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sup := w.c.Supervisor
	t0 := time.Now()
	sup.autoStop(t.Context(), t0)

	// A job holding the instance defers the stop rather than dropping it.
	held := seedStaleJob(t, db, jobs.KindBackup.String(), "", "{}")
	sup.autoStop(t.Context(), t0.Add(5*time.Minute))
	if n := countStops(t, db); n != 0 {
		t.Fatalf("stop jobs = %d while the lock was held, want 0", n)
	}
	seed(t, db, `DELETE FROM job_locks WHERE job_id = ?`, held)
	seed(t, db, `DELETE FROM job_runs WHERE id = ?`, held)

	sup.autoStop(t.Context(), t0.Add(5*time.Minute+10*time.Second))
	if n := countStops(t, db); n != 1 {
		t.Fatalf("stop jobs = %d, want 1", n)
	}
	var actor, detail string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT actor_name, detail FROM audit_log WHERE action = 'instances.stop'`).Scan(&actor, &detail); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actor != "Auto-stop" || detail != `{"reason":"no_players","minutes":5}` {
		t.Errorf("audit row = %q %s", actor, detail)
	}
}

// countStops counts the stop jobs submitted for the seeded instance.
func countStops(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_runs WHERE kind = ? AND instance_id = ?`,
		jobs.KindStop.String(), seededInstanceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
