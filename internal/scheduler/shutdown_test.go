package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// seedServer inserts an instance in state, with a container unless containerID is empty.
func seedServer(t *testing.T, db *store.DB, id, state, containerID string, basePort int) {
	t.Helper()
	var container *string
	if containerID != "" {
		container = &containerID
	}
	if _, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO instances (
			id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		id, id, state, container, "/srv/"+id, basePort, id, id, "cp-"+id, store.Now(), store.Now(),
	); err != nil {
		t.Fatalf("seed instance %s: %v", id, err)
	}
}

// shutdownClock is a clock over db whose hooks record what they were asked to do. warnOK and
// stopErr script the hooks' answers.
type shutdownClock struct {
	*Shutdowns
	mu      sync.Mutex
	warned  []string
	stopped []string
	warnOK  bool
	stopErr error
}

// tick runs one tick and waits for the warnings it sent.
func (c *shutdownClock) tick(t *testing.T, now time.Time) {
	t.Helper()
	c.Tick(t.Context(), now)
	c.sends.wait()
}

func newShutdownClock(db *store.DB) *shutdownClock {
	c := &shutdownClock{warnOK: true}
	c.Shutdowns = &Shutdowns{
		DB: db,
		Stop: func(_ context.Context, inst *store.Instance, _ time.Time) error {
			c.stopped = append(c.stopped, inst.ID)
			return c.stopErr
		},
		Warn: func(_ context.Context, inst *store.Instance, _ time.Duration) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.warned = append(c.warned, inst.ID)
			return c.warnOK
		},
	}
	return c
}

func seedPowerCut(t *testing.T, db *store.DB, id string, at time.Time) {
	t.Helper()
	if err := db.CreatePlannedShutdown(t.Context(), &store.PlannedShutdown{ID: id, PowerOffAt: at}, nil); err != nil {
		t.Fatalf("CreatePlannedShutdown: %v", err)
	}
}

// TestShutdownTickActsInItsWindows asserts nothing happens before the warning lead, only running
// servers are warned inside it, only running servers are stopped from the stop lead, and nothing
// happens once the power cut has passed.
func TestShutdownTickActsInItsWindows(t *testing.T) {
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	tests := []struct {
		name                string
		now                 time.Time
		wantWarn, wantStops []string
	}{
		{"before the warning", cut.Add(-testWarnLead - time.Second), nil, nil},
		{"warning", cut.Add(-testWarnLead), []string{"running"}, nil},
		{"stopping", cut.Add(-testStopLead), nil, []string{"running"}},
		{"just before the cut", cut.Add(-time.Second), nil, []string{"running"}},
		{"after the cut", cut, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := open(t)
			seedServer(t, db, "running", "running", "c1", 2456)
			seedServer(t, db, "stopped", "stopped", "c2", 2466)
			seedServer(t, db, "backing", "backing_up", "c3", 2476)
			seedPowerCut(t, db, "cut", cut)
			c := newShutdownClock(db)
			c.tick(t, tt.now)
			if !slices.Equal(c.warned, tt.wantWarn) || !slices.Equal(c.stopped, tt.wantStops) {
				t.Fatalf("warned %v, stopped %v; want %v, %v", c.warned, c.stopped, tt.wantWarn, tt.wantStops)
			}
		})
	}
}

// TestShutdownWarnsOnceAndRetriesAFailedWarning asserts a delivered warning is not repeated
// for the same power cut, and an undelivered one is sent again on the next tick.
func TestShutdownWarnsOnceAndRetriesAFailedWarning(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	now := cut.Add(-testWarnLead)

	c.warnOK = false
	c.tick(t, now)
	c.warnOK = true
	c.tick(t, now.Add(10*time.Second))
	c.tick(t, now.Add(20*time.Second))
	if !slices.Equal(c.warned, []string{"a", "a"}) {
		t.Fatalf("warned %v, want one failed and one delivered warning", c.warned)
	}
}

// TestAWarningIsNotRepeatedAfterARestart asserts that a clock started again inside the warning
// window, after a tick has recorded the warning, does not warn players a second time of the
// same power cut.
func TestAWarningIsNotRepeatedAfterARestart(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	now := cut.Add(-testWarnLead)

	first := newShutdownClock(db)
	first.tick(t, now)
	first.tick(t, now.Add(5*time.Second))
	restarted := newShutdownClock(db)
	restarted.tick(t, now.Add(10*time.Second))

	if len(first.warned) != 1 || len(restarted.warned) != 0 {
		t.Errorf(
			"warned %v before the restart and %v after it, want one warning in all",
			first.warned,
			restarted.warned,
		)
	}
}

// TestAWarningEndsByTheStopTime asserts a warning's context expires no later than the moment
// the servers stop, so a slow server cannot hold up the stops.
func TestAWarningEndsByTheStopTime(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(testWarnLead - time.Second)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	var deadline time.Time
	c.Warn = func(ctx context.Context, _ *store.Instance, _ time.Duration) bool {
		deadline, _ = ctx.Deadline()
		return true
	}
	c.tick(t, time.Now().UTC())
	if stopAt := cut.Add(-testStopLead); deadline.IsZero() || deadline.After(stopAt.Add(time.Second)) {
		t.Fatalf("warning deadline = %v, want by %v", deadline, stopAt)
	}
}

// TestShutdownRetriesABusyServer asserts a stop refused by a lock conflict is submitted again on
// the next tick.
func TestShutdownRetriesABusyServer(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	c.stopErr = &store.JobConflict{JobID: "j", Kind: "backup"}
	now := cut.Add(-testStopLead)

	c.tick(t, now)
	c.stopErr = nil
	c.tick(t, now.Add(10*time.Second))
	if !slices.Equal(c.stopped, []string{"a", "a"}) {
		t.Fatalf("stopped %v, want a retry after the conflict", c.stopped)
	}
}

// TestShutdownActsOnlyOnTheSoonestCut asserts a later power cut inside its own warning window
// does not warn while an earlier one is pending.
func TestShutdownActsOnlyOnTheSoonestCut(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "first", cut)
	seedPowerCut(t, db, "second", cut.Add(3*time.Minute))
	c := newShutdownClock(db)

	c.tick(t, cut.Add(-testWarnLead))
	c.tick(t, cut.Add(-testWarnLead+time.Minute))
	if !slices.Equal(c.warned, []string{"a"}) {
		t.Fatalf("warned %v, want a single warning", c.warned)
	}
}

// TestShutdownAnnouncesOncePerCut asserts the notification goes out once, at the warning lead,
// naming the servers running then.
func TestShutdownAnnouncesOncePerCut(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "alpha", "running", "c1", 2456)
	seedServer(t, db, "bravo", "stopped", "c2", 2466)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	var announced [][]string
	c.Announce = func(_ context.Context, powerOff, stopAt time.Time, running []string) {
		if !powerOff.Equal(cut) || !stopAt.Equal(cut.Add(-testStopLead)) {
			t.Errorf("announced %v, %v", powerOff, stopAt)
		}
		announced = append(announced, running)
	}

	c.tick(t, cut.Add(-testWarnLead-time.Second))
	c.tick(t, cut.Add(-testWarnLead))
	c.tick(t, cut.Add(-testStopLead))
	if len(announced) != 1 || !slices.Equal(announced[0], []string{"alpha"}) {
		t.Fatalf("announced %v, want one notification naming alpha", announced)
	}
}

// TestShutdownAnnouncesOnlyWhileAServerRuns asserts no notification goes out while every server
// is stopped, and that one started before the power cut is announced then.
func TestShutdownAnnouncesOnlyWhileAServerRuns(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "alpha", "stopped", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	var announced [][]string
	c.Announce = func(_ context.Context, _, _ time.Time, running []string) {
		announced = append(announced, running)
	}

	c.tick(t, cut.Add(-testWarnLead))
	if len(announced) != 0 {
		t.Fatalf("announced %v with no server running", announced)
	}
	if _, err := db.Writer.ExecContext(
		t.Context(),
		`UPDATE instances SET state = 'running' WHERE id = 'alpha'`,
	); err != nil {
		t.Fatal(err)
	}
	c.tick(t, cut.Add(-testStopLead))
	c.tick(t, cut.Add(-time.Minute))
	if len(announced) != 1 || !slices.Equal(announced[0], []string{"alpha"}) {
		t.Fatalf("announced %v, want one notification naming alpha", announced)
	}
}

// TestPowerOffTime asserts which typed times are read, in the given zone, and which are refused.
func TestPowerOffTime(t *testing.T) {
	kyiv, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 13, 30, 0, 0, kyiv)
	tests := []struct {
		text, zone string
		want       time.Time
		wantErr    error
	}{
		{"14:00", "Europe/Kyiv", time.Date(2026, 10, 8, 14, 0, 0, 0, kyiv), nil},
		{" 09:15 ", "Europe/Kyiv", time.Date(2026, 10, 9, 9, 15, 0, 0, kyiv), nil},
		{"13:30", "Europe/Kyiv", time.Date(2026, 10, 9, 13, 30, 0, 0, kyiv), nil},
		{"2026-10-10 06:00", "Europe/Kyiv", time.Date(2026, 10, 10, 6, 0, 0, 0, kyiv), nil},
		{"2026-10-10T06:00", "Europe/Kyiv", time.Date(2026, 10, 10, 6, 0, 0, 0, kyiv), nil},
		{"14:00", "UTC", time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC), nil},
		{"2026-10-08 13:00", "Europe/Kyiv", time.Time{}, ErrPastPowerOffTime},
		{"tomorrow", "Europe/Kyiv", time.Time{}, ErrBadPowerOffTime},
		{"25:00", "Europe/Kyiv", time.Time{}, ErrBadPowerOffTime},
		{"14:00", "Mars/Olympus", time.Time{}, ErrBadPowerOffTime},
		{"14:00", "", time.Time{}, ErrBadPowerOffTime},
	}
	for _, tt := range tests {
		got, err := PowerOffTime(tt.text, tt.zone, now)
		if !errors.Is(err, tt.wantErr) || !got.Equal(tt.want) {
			t.Errorf("PowerOffTime(%q, %q) = %v, %v; want %v, %v", tt.text, tt.zone, got, err, tt.want, tt.wantErr)
		}
	}
}

// Asserts the stops start early enough that a stop using the whole stop timeout, submitted up
// to one tick into the stop window, still ends before the power goes off.
func TestServersStopEarlyEnoughForTheStopTimeout(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	c.StopTimeout = 5 * time.Minute
	c.Interval = 10 * time.Second

	c.tick(t, cut.Add(-c.StopTimeout-c.Interval))
	if !slices.Equal(c.stopped, []string{"a"}) {
		t.Fatalf("stopped %v %v before the cut, want a: a stop taking the whole timeout would run into the cut",
			c.stopped, c.StopTimeout+c.Interval)
	}
}

// Asserts a tick does not wait for slow warnings to several servers, and that they all end by
// the stop time together rather than each taking the whole time left.
func TestSlowWarningsDoNotDelayTheStops(t *testing.T) {
	db := open(t)
	for i, id := range []string{"a", "b", "c"} {
		seedServer(t, db, id, "running", "c"+id, 2456+10*i)
	}
	c := newShutdownClock(db)
	cut := time.Now().UTC().Add(c.StopLead() + 500*time.Millisecond)
	seedPowerCut(t, db, "cut", cut)
	c.Warn = func(ctx context.Context, _ *store.Instance, _ time.Duration) bool {
		<-ctx.Done()
		return false
	}

	start := time.Now()
	c.Tick(t.Context(), start.UTC())
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Fatalf("the warning tick took %v, want it to leave the warnings running", took)
	}
	c.sends.wait()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the warnings took %v, want them over by the stop time, 500ms away", took)
	}
}

// The leads of a clock with no stop timeout or tick, which every test clock is unless it sets
// them.
var (
	testStopLead = (&Shutdowns{}).StopLead()
	testWarnLead = (&Shutdowns{}).WarnLead()
)
