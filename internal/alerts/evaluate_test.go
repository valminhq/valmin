package alerts

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func base() Snapshot { return Snapshot{Now: now} }

// has returns the one condition of kind k for instance id. More than one is a failure: the
// unique index rejects a duplicate and an operator would be told twice.
func has(t *testing.T, got []Condition, k Kind, id string) (Condition, bool) {
	t.Helper()
	var found []Condition
	for _, c := range got {
		if c.Kind == k && c.InstanceID == id {
			found = append(found, c)
		}
	}
	if len(found) > 1 {
		t.Fatalf("%s for %q produced %d conditions, want at most 1", k, id, len(found))
	}
	if len(found) == 0 {
		return Condition{}, false
	}
	return found[0], true
}

// TestLowDiskIsHostLevelAndRespectsTheFloor asserts the condition carries no instance and
// fires only strictly below the floor.
func TestLowDiskIsHostLevelAndRespectsTheFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		free, alarm uint64
		want        bool
	}{
		{"below the floor", 100, 200, true},
		{"exactly at the floor", 200, 200, false},
		{"above the floor", 300, 200, false},
		{"no floor configured", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.FreeBytes, s.AlarmBytes = tc.free, tc.alarm
			if _, ok := has(t, Evaluate(&s, nil), KindLowDisk, ""); ok != tc.want {
				t.Errorf("low disk = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestInstanceRowConditions asserts the three conditions read straight off an instance row.
func TestInstanceRowConditions(t *testing.T) {
	t.Parallel()
	s := base()
	s.Instances = []store.Instance{
		{ID: "a", RestartRequired: true, State: "running"},
		{ID: "b", State: "error"},
		{ID: "c", State: "stopped"},
	}
	got := Evaluate(&s, nil)

	if _, ok := has(t, got, KindRestartRequired, "a"); !ok {
		t.Error("a wants a restart-required condition")
	}
	if _, ok := has(t, got, KindInstanceError, "b"); !ok {
		t.Error("b is in error and wants a condition")
	}
	for _, k := range []Kind{KindRestartRequired, KindInstanceError} {
		if _, ok := has(t, got, k, "c"); ok {
			t.Errorf("a healthy instance produced %s", k)
		}
	}
}

// TestUpdateAvailableComparesKnownBuildsOnly asserts a legacy cache alias never reports an
// update that does not exist.
func TestUpdateAvailableComparesKnownBuildsOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		installed, public string
		want              bool
	}{
		{"behind the branch", "100", "200", true},
		{"up to date", "200", "200", false},
		{"installed is a cache alias", "steamapps-cache", "200", false},
		{"nothing observed yet", "100", "", false},
		{"never provisioned", "", "200", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.Instances = []store.Instance{{ID: "a"}}
			s.InstalledBuilds = map[string]string{"a": tc.installed}
			s.PublicBuild = tc.public
			if _, ok := has(t, Evaluate(&s, nil), KindUpdateAvailable, "a"); ok != tc.want {
				t.Errorf("update available = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestFailedJobsTrustTheSupersededInput asserts the evaluator reports the failures it is
// handed, including global ones, and carries the failing kind and code.
func TestFailedJobsTrustTheSupersededInput(t *testing.T) {
	t.Parallel()
	s := base()
	s.LatestTerminalJobs = []store.Job{
		{Kind: "backup", Status: jobs.StatusFailed, InstanceID: new("a"), ErrorCode: new("disk_full")},
		{Kind: "start", Status: jobs.StatusSucceeded, InstanceID: new("a")},
		{Kind: "prune", Status: jobs.StatusFailed},
	}
	got := Evaluate(&s, nil)

	c, ok := has(t, got, KindJobFailed, "a")
	if !ok {
		t.Fatal("a failed backup wants a condition")
	}
	if c.Detail["Job"] != "backup" || c.Detail["Error"] != "disk_full" {
		t.Errorf("detail = %v, want the failing kind and its error code", c.Detail)
	}
	// A global job carries no instance and still has to be reported: nothing else shows it,
	// because it has no instance page to appear on.
	if _, ok := has(t, got, KindJobFailed, ""); !ok {
		t.Error("a failed global job wants a condition")
	}
}

// TestUncleanStopNeedsAnExplicitFalse asserts a missing clean flag is not read as unclean:
// most kinds have no such concept (12 §3.4).
func TestUncleanStopNeedsAnExplicitFalse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		clean *bool
		want  bool
	}{
		{"stopped without finishing the save", new(false), true},
		{"stopped cleanly", new(true), false},
		{"kind carries no clean flag", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.CleanSignals = []store.Job{{Kind: "stop", InstanceID: new("a"), Clean: tc.clean}}
			if _, ok := has(t, Evaluate(&s, nil), KindUncleanStop, "a"); ok != tc.want {
				t.Errorf("unclean stop = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestStaleBackupIsScheduleAware asserts the condition fires only for an instance with a
// ticking schedule, and only past the tolerated multiple of its interval.
func TestStaleBackupIsScheduleAware(t *testing.T) {
	t.Parallel()
	hourly := func(lastRun *time.Time) BackupSchedule {
		return BackupSchedule{InstanceID: "a", Interval: time.Hour, LastRunAt: lastRun}
	}
	ran := now.Add(-3 * time.Hour)

	for _, tc := range []struct {
		name       string
		schedules  []BackupSchedule
		lastBackup *time.Time
		want       bool
	}{
		{"no schedule at all", nil, new(now.Add(-72 * time.Hour)), false},
		{"schedule has never ticked", []BackupSchedule{hourly(nil)}, nil, false},
		{"backup within tolerance", []BackupSchedule{hourly(&ran)}, new(now.Add(-90 * time.Minute)), false},
		{"backup past tolerance", []BackupSchedule{hourly(&ran)}, new(now.Add(-150 * time.Minute)), true},
		{"schedule ticking but no backup ever", []BackupSchedule{hourly(&ran)}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.BackupSchedules = tc.schedules
			if tc.lastBackup != nil {
				s.LastBackups = map[string]time.Time{"a": *tc.lastBackup}
			}
			if _, ok := has(t, Evaluate(&s, nil), KindStaleBackup, "a"); ok != tc.want {
				t.Errorf("stale backup = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestCrashLoopCountsOnlyInsideTheWindow asserts old incidents age out of the count.
func TestCrashLoopCountsOnlyInsideTheWindow(t *testing.T) {
	t.Parallel()
	recent := []time.Time{now.Add(-5 * time.Minute), now.Add(-10 * time.Minute), now.Add(-15 * time.Minute)}

	for _, tc := range []struct {
		name      string
		incidents []time.Time
		want      bool
	}{
		{"three inside the window", recent, true},
		{"two inside the window", recent[:2], false},
		{"three but all older than the window", []time.Time{
			now.Add(-2 * time.Hour), now.Add(-3 * time.Hour), now.Add(-4 * time.Hour),
		}, false},
		{"never crashed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.Instances = []store.Instance{{ID: "a"}}
			s.Incidents = map[string][]time.Time{"a": tc.incidents}
			if _, ok := has(t, Evaluate(&s, nil), KindCrashLoop, "a"); ok != tc.want {
				t.Errorf("crash loop = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestJobStuckNeedsAStartTime asserts a job that has not started is never reported as
// overrunning.
func TestJobStuckNeedsAStartTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		started *time.Time
		want    bool
	}{
		{"running well past the threshold", new(now.Add(-3 * time.Hour)), true},
		{"running inside the threshold", new(now.Add(-10 * time.Minute)), false},
		{"not started yet", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			s.RunningJobs = []store.Job{{Kind: "backup", InstanceID: new("a"), StartedAt: tc.started}}
			if _, ok := has(t, Evaluate(&s, nil), KindJobStuck, "a"); ok != tc.want {
				t.Errorf("job stuck = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestResolverTunesOneInstanceWithoutTheOthers asserts a per-instance threshold applies to
// that instance alone.
func TestResolverTunesOneInstanceWithoutTheOthers(t *testing.T) {
	t.Parallel()
	s := base()
	s.RunningJobs = []store.Job{
		{Kind: "backup", InstanceID: new("noisy"), StartedAt: new(now.Add(-30 * time.Minute))},
		{Kind: "backup", InstanceID: new("quiet"), StartedAt: new(now.Add(-30 * time.Minute))},
	}
	got := Evaluate(&s, func(_ Kind, id string) Params {
		if id == "noisy" {
			return Params{StuckAfter: 10 * time.Minute}
		}
		return Params{}
	})

	if _, ok := has(t, got, KindJobStuck, "noisy"); !ok {
		t.Error("the tightened instance wants a stuck-job condition")
	}
	if _, ok := has(t, got, KindJobStuck, "quiet"); ok {
		t.Error("the default instance is inside the default threshold and wants none")
	}
}

// TestDefaultsAreAppliedToUnsetFieldsOnly asserts setting one threshold does not zero the rest.
func TestDefaultsAreAppliedToUnsetFieldsOnly(t *testing.T) {
	t.Parallel()
	p := Params{CrashCount: 9}.Defaults()
	if p.CrashCount != 9 {
		t.Errorf("CrashCount = %d, want the configured 9", p.CrashCount)
	}
	if p.StuckAfter != time.Hour || p.CrashWindow != 30*time.Minute || p.StaleFactor != 2 {
		t.Errorf("unset fields = %+v, want the documented defaults", p)
	}
}

// TestParseKindRejectsWhatNothingEvaluates guards the closed registry.
func TestParseKindRejectsWhatNothingEvaluates(t *testing.T) {
	t.Parallel()
	if _, ok := ParseKind("low_disk"); !ok {
		t.Error("low_disk is a real kind")
	}
	if _, ok := ParseKind("world_on_fire"); ok {
		t.Error("an unknown kind resolved")
	}
	if len(Kinds()) != len(all) {
		t.Error("Kinds() must report every registered kind")
	}
}
