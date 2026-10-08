package scheduler

import (
	"context"
	"errors"
	"slices"
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
	warned  []string
	stopped []string
	warnOK  bool
	stopErr error
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
		{"before the warning", cut.Add(-ShutdownWarnLead - time.Second), nil, nil},
		{"warning", cut.Add(-ShutdownWarnLead), []string{"running"}, nil},
		{"stopping", cut.Add(-ShutdownStopLead), nil, []string{"running"}},
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
			c.Tick(t.Context(), tt.now)
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
	now := cut.Add(-ShutdownWarnLead)

	c.warnOK = false
	c.Tick(t.Context(), now)
	c.warnOK = true
	c.Tick(t.Context(), now.Add(10*time.Second))
	c.Tick(t.Context(), now.Add(20*time.Second))
	if !slices.Equal(c.warned, []string{"a", "a"}) {
		t.Fatalf("warned %v, want one failed and one delivered warning", c.warned)
	}
}

// TestAWarningEndsByTheStopTime asserts a warning's context expires no later than the moment
// the servers stop, so a slow server cannot hold up the stops.
func TestAWarningEndsByTheStopTime(t *testing.T) {
	db := open(t)
	cut := time.Now().UTC().Add(ShutdownWarnLead - time.Second)
	seedServer(t, db, "a", "running", "c1", 2456)
	seedPowerCut(t, db, "cut", cut)
	c := newShutdownClock(db)
	var deadline time.Time
	c.Warn = func(ctx context.Context, _ *store.Instance, _ time.Duration) bool {
		deadline, _ = ctx.Deadline()
		return true
	}
	c.Tick(t.Context(), time.Now().UTC())
	if stopAt := cut.Add(-ShutdownStopLead); deadline.IsZero() || deadline.After(stopAt.Add(time.Second)) {
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
	now := cut.Add(-ShutdownStopLead)

	c.Tick(t.Context(), now)
	c.stopErr = nil
	c.Tick(t.Context(), now.Add(10*time.Second))
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

	c.Tick(t.Context(), cut.Add(-ShutdownWarnLead))
	c.Tick(t.Context(), cut.Add(-ShutdownWarnLead+time.Minute))
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
		if !powerOff.Equal(cut) || !stopAt.Equal(cut.Add(-ShutdownStopLead)) {
			t.Errorf("announced %v, %v", powerOff, stopAt)
		}
		announced = append(announced, running)
	}

	c.Tick(t.Context(), cut.Add(-ShutdownWarnLead-time.Second))
	c.Tick(t.Context(), cut.Add(-ShutdownWarnLead))
	c.Tick(t.Context(), cut.Add(-ShutdownStopLead))
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

	c.Tick(t.Context(), cut.Add(-ShutdownWarnLead))
	if len(announced) != 0 {
		t.Fatalf("announced %v with no server running", announced)
	}
	if _, err := db.Writer.ExecContext(
		t.Context(),
		`UPDATE instances SET state = 'running' WHERE id = 'alpha'`,
	); err != nil {
		t.Fatal(err)
	}
	c.Tick(t.Context(), cut.Add(-ShutdownStopLead))
	c.Tick(t.Context(), cut.Add(-time.Minute))
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
