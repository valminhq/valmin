package api

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
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
