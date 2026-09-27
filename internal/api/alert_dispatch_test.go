package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

func at(hour int) time.Time {
	return time.Date(2026, 9, 19, hour, 0, 0, 0, time.UTC)
}

func quietRule(start, end int, tz string) *store.AlertRule {
	return &store.AlertRule{QuietStart: &start, QuietEnd: &end, QuietTZ: &tz}
}

// TestQuietWindowsCoverMidnight asserts a window whose start is above its end wraps, which is
// the shape every overnight window has.
func TestQuietWindowsCoverMidnight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rule *store.AlertRule
		now  time.Time
		want bool
	}{
		{"inside a daytime window", quietRule(540, 1020, "UTC"), at(12), true},
		{"before a daytime window", quietRule(540, 1020, "UTC"), at(8), false},
		{"on the closing edge", quietRule(540, 1020, "UTC"), at(17), false},
		{"late inside an overnight window", quietRule(1320, 420, "UTC"), at(23), true},
		{"early inside an overnight window", quietRule(1320, 420, "UTC"), at(3), true},
		{"outside an overnight window", quietRule(1320, 420, "UTC"), at(12), false},
		{"no window at all", &store.AlertRule{}, at(3), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := quiet(tc.rule, tc.now); got != tc.want {
				t.Errorf("quiet = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQuietWindowsAreReadInTheRuleTimezone asserts the window means local time, not the
// daemon's.
func TestQuietWindowsAreReadInTheRuleTimezone(t *testing.T) {
	t.Parallel()
	rule := quietRule(0, 420, "Asia/Tokyo")
	// 22:00 UTC is 07:00 the next day in Tokyo, past the window's end.
	if quiet(rule, at(22)) {
		t.Error("07:00 Tokyo is outside a window ending at 07:00")
	}
	// 18:00 UTC is 03:00 Tokyo, inside it.
	if !quiet(rule, at(18)) {
		t.Error("03:00 Tokyo is inside a window from midnight to 07:00")
	}
}

// TestRuleMatchingCoversInstanceAndGlobalScope asserts a rule with no instance covers every
// condition of its kind, and a disabled rule covers none.
func TestRuleMatchingCoversInstanceAndGlobalScope(t *testing.T) {
	t.Parallel()
	instanceA, instanceB := "inst-a", "inst-b"
	conditionA := &store.AlertCondition{Kind: "low_disk", InstanceID: &instanceA}
	hostWide := &store.AlertCondition{Kind: "low_disk"}

	for _, tc := range []struct {
		name      string
		rule      store.AlertRule
		condition *store.AlertCondition
		want      bool
	}{
		{"all instances", store.AlertRule{ConditionKind: "low_disk", Enabled: true}, conditionA, true},
		{
			"all instances covers a host condition",
			store.AlertRule{ConditionKind: "low_disk", Enabled: true},
			hostWide, true,
		},
		{
			"the named instance",
			store.AlertRule{ConditionKind: "low_disk", Enabled: true, InstanceID: &instanceA},
			conditionA, true,
		},
		{
			"another instance",
			store.AlertRule{ConditionKind: "low_disk", Enabled: true, InstanceID: &instanceB},
			conditionA, false,
		},
		{
			"an instance rule never covers a host condition",
			store.AlertRule{ConditionKind: "low_disk", Enabled: true, InstanceID: &instanceA},
			hostWide, false,
		},
		{
			"another kind",
			store.AlertRule{ConditionKind: "crash_loop", Enabled: true},
			conditionA, false,
		},
		{"disabled", store.AlertRule{ConditionKind: "low_disk"}, conditionA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matches(&tc.rule, tc.condition); got != tc.want {
				t.Errorf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestThresholdsPreferTheInstanceRule asserts a rule naming an instance overrides one covering
// all of them, which is what tuning a single noisy server depends on.
func TestThresholdsPreferTheInstanceRule(t *testing.T) {
	t.Parallel()
	noisy := "inst-a"
	resolve := thresholds([]store.AlertRule{
		{ConditionKind: "job_stuck", Enabled: true, Params: `{"stuck_after_seconds":3600}`},
		{ConditionKind: "job_stuck", Enabled: true, InstanceID: &noisy, Params: `{"stuck_after_seconds":60}`},
	})

	if got := resolve(alerts.KindJobStuck, "inst-a").StuckAfter; got != time.Minute {
		t.Errorf("tuned instance = %v, want 1m", got)
	}
	if got := resolve(alerts.KindJobStuck, "inst-b").StuckAfter; got != time.Hour {
		t.Errorf("untuned instance = %v, want the all-instances 1h", got)
	}
}

// TestParamsRoundTripInSeconds asserts the wire form is seconds, per 11 §1's unit-in-the-name
// rule, and that an unreadable rule falls back to the defaults rather than to zero.
func TestParamsRoundTripInSeconds(t *testing.T) {
	t.Parallel()
	raw, err := encodeParams(paramsWire{CrashCount: 5, CrashWindowSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeParams(raw)
	if got.CrashCount != 5 || got.CrashWindow != 15*time.Minute {
		t.Errorf("decoded = %+v, want 5 crashes in 15m", got)
	}
	if defaults := decodeParams("not json").Defaults(); defaults.CrashCount != 3 {
		t.Errorf("unreadable params = %+v, want the defaults", defaults)
	}
}

// setQuietHours puts a rule inside a quiet window around the current time, or takes its window
// away, which is how a test stands in for the window opening and closing.
func setQuietHours(t *testing.T, db *store.DB, ruleID string, on bool) {
	t.Helper()
	if !on {
		seed(t, db, `UPDATE alert_rules SET quiet_start = NULL, quiet_end = NULL, quiet_tz = NULL
			WHERE id = ?`, ruleID)
		return
	}
	now := time.Now().UTC()
	minutes := now.Hour()*60 + now.Minute()
	seed(t, db, `UPDATE alert_rules SET quiet_start = ?, quiet_end = ?, quiet_tz = 'UTC' WHERE id = ?`,
		(minutes+1440-60)%1440, (minutes+60)%1440, ruleID)
}

// TestQuietHoursHoldAlertsRatherThanDropThem asserts an edge a rule skipped inside its quiet
// window is sent by the first scan after the window ends, that a clearing is only ever sent for
// an alert the rule announced, and that a clearing is never sent while its condition is open again.
func TestQuietHoursHoldAlertsRatherThanDropThem(t *testing.T) {
	opened, resolved := notify.KindAlertOpened.String(), notify.KindAlertResolved.String()
	for _, tc := range []struct {
		name  string
		steps []string
		want  map[string]int
	}{
		{
			"an alert raised in quiet hours is sent once they end",
			[]string{"quiet", "fail", "scan", "loud", "scan", "scan"},
			map[string]int{opened: 1},
		},
		{
			"a clearing in quiet hours is sent once they end",
			[]string{"fail", "scan", "quiet", "recover", "scan", "loud", "scan", "scan"},
			map[string]int{opened: 1, resolved: 1},
		},
		{
			"an alert never announced is never cleared",
			[]string{"quiet", "fail", "scan", "recover", "loud", "scan", "scan"},
			map[string]int{},
		},
		{
			"an alert outside quiet hours is sent and cleared at once",
			[]string{"fail", "scan", "recover", "scan", "scan"},
			map[string]int{opened: 1, resolved: 1},
		},
		{
			"an alert that clears and returns in quiet hours is never sent as cleared",
			[]string{"fail", "scan", "quiet", "recover", "scan", "fail", "scan", "loud", "scan", "scan"},
			map[string]int{opened: 2},
		},
		{
			"a clearing held behind a returned alert is sent once that alert clears",
			[]string{"fail", "scan", "quiet", "recover", "scan", "fail", "scan", "loud", "scan", "recover", "scan"},
			map[string]int{opened: 2, resolved: 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, _, _ := provisionWorld(t)
			recordingReceiver(t, rt, http.StatusNoContent)
			ruled := seedWebhook(t, db, "ruled")
			ruleID := seedRule(t, db, alerts.KindJobFailed, "", ruled)
			instanceID := seedStoppedInstance(t, db, "night-shift").ID

			for _, step := range tc.steps {
				switch step {
				case "quiet", "loud":
					setQuietHours(t, db, ruleID, step == "quiet")
				case "fail":
					failJob(t, rt, db, jobs.KindBackup, instanceID)
				case "recover":
					seed(t, db, `INSERT INTO job_runs (id, kind, status, lock_key, instance_id,
						instance_name, created_at, started_at, finished_at)
						VALUES (?, ?, 'succeeded', ?, ?, 'Midgard', ?, ?, ?)`,
						store.NewID(), jobs.KindBackup.String(), jobs.InstanceLockKey(instanceID),
						instanceID, store.Now(), store.Now(), store.Now())
				case "scan":
					scanNow(t, rt)
				}
			}
			assertDeliveries(t, db, "the rule's destination", ruled, tc.want)
		})
	}
}

// TestStaleBackupsCountOnlyTheArchivesACadenceOwes asserts a hot copy never stands in for a
// scheduled backup, and that a restart schedule on a server that archives on restart is a
// backup cadence while its restarts run, and owes nothing while they are skipped.
func TestStaleBackupsCountOnlyTheArchivesACadenceOwes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        jobs.Kind
		onRestart   bool
		lastRestart string
		archive     string
		want        bool
	}{
		{"a recent hot copy under a backup schedule", jobs.KindBackup, false, "", "hot", true},
		{"a recent archive under a backup schedule", jobs.KindBackup, false, "", "consistent", false},
		{"a restart that ran and archived nothing", jobs.KindRestart, true, "succeeded", "", true},
		{"a restart that ran and archived", jobs.KindRestart, true, "succeeded", "consistent", false},
		{"a restart skipped because the server is stopped", jobs.KindRestart, true, "cancelled", "", false},
		{"a restart schedule that archives nothing", jobs.KindRestart, false, "succeeded", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, db, _, _ := provisionWorld(t)
			inst := seedStoppedInstance(t, db, "cadence")
			seed(t, db, `UPDATE instances SET backup_on_restart = ? WHERE id = ?`, tc.onRestart, inst.ID)
			now := time.Now().UTC()
			seed(t, db, `INSERT INTO scheduled_jobs
				(id, instance_id, kind, cron, payload, enabled, last_run_at, next_run_at)
				VALUES (?, ?, ?, '@hourly', '{}', TRUE, ?, ?)`,
				store.NewID(), inst.ID, tc.kind.String(),
				store.FormatTime(now.Add(-30*time.Minute)), store.FormatTime(now.Add(30*time.Minute)))
			if tc.lastRestart != "" {
				seed(t, db, `INSERT INTO job_runs (id, kind, status, lock_key, instance_id,
					instance_name, created_at, started_at, finished_at)
					VALUES (?, ?, ?, ?, ?, 'cadence', ?, ?, ?)`,
					store.NewID(), jobs.KindRestart.String(), tc.lastRestart,
					jobs.InstanceLockKey(inst.ID), inst.ID, store.Now(), store.Now(), store.Now())
			}
			if tc.archive != "" {
				if err := db.CreateBackup(t.Context(), &store.Backup{
					ID: store.NewID(), InstanceID: inst.ID, Path: "/dev/null", SizeBytes: 1,
					SHA256: "abc123", WorldName: "W", Trigger: store.TriggerManual,
					Consistent: tc.archive == "consistent",
				}); err != nil {
					t.Fatal(err)
				}
			}

			scanNow(t, rt)

			open, err := db.OpenConditions(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			stale := false
			for i := range open {
				if open[i].Kind == alerts.KindStaleBackup.String() && deref(open[i].InstanceID) == inst.ID {
					stale = true
				}
			}
			if stale != tc.want {
				t.Errorf("stale backup = %v, want %v", stale, tc.want)
			}
		})
	}
}
