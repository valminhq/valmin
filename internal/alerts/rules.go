package alerts

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// ParamsWire is the stored and API JSON form of a rule's thresholds. Durations are whole
// seconds with the unit in the field name.
type ParamsWire struct {
	CrashCount         int     `json:"crash_count,omitempty"`
	CrashWindowSeconds int     `json:"crash_window_seconds,omitempty"`
	StuckAfterSeconds  int     `json:"stuck_after_seconds,omitempty"`
	StaleFactor        float64 `json:"stale_factor,omitempty"`
}

// ParamsWireOf reads a rule's stored thresholds. Unreadable JSON yields the zero value, which
// means the defaults: one bad rule must not stop every condition being evaluated.
func ParamsWireOf(raw string) ParamsWire {
	var w ParamsWire
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return ParamsWire{}
		}
	}
	return w
}

// EncodeParams renders thresholds for storage.
func EncodeParams(w ParamsWire) (string, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encode alert thresholds: %w", err)
	}
	return string(raw), nil
}

// DecodeParams reads a rule's stored thresholds as typed params.
func DecodeParams(raw string) Params {
	w := ParamsWireOf(raw)
	return Params{
		CrashCount:  w.CrashCount,
		CrashWindow: time.Duration(w.CrashWindowSeconds) * time.Second,
		StuckAfter:  time.Duration(w.StuckAfterSeconds) * time.Second,
		StaleFactor: w.StaleFactor,
	}
}

// RuleResolver resolves a kind's params for one instance. The most specific enabled rule wins, so
// a rule naming the instance overrides one covering all of them.
func RuleResolver(rules []store.AlertRule) Resolver {
	return func(kind Kind, instanceID string) Params {
		var chosen *store.AlertRule
		for i := range rules {
			r := &rules[i]
			if !r.Enabled || r.ConditionKind != kind.String() {
				continue
			}
			if r.InstanceID != nil && *r.InstanceID != instanceID {
				continue
			}
			if chosen == nil || (chosen.InstanceID == nil && r.InstanceID != nil) {
				chosen = r
			}
		}
		if chosen == nil {
			return Params{}
		}
		return DecodeParams(chosen.Params)
	}
}

// Matches reports whether a rule covers this condition. A rule with no instance covers every
// one, including a host-level condition.
func Matches(r *store.AlertRule, c *store.AlertCondition) bool {
	if !r.Enabled || r.ConditionKind != c.Kind {
		return false
	}
	if r.InstanceID == nil {
		return true
	}
	return c.InstanceID != nil && *c.InstanceID == *r.InstanceID
}

// Quiet reports whether now falls inside a rule's quiet window, in the rule's own timezone. A
// window whose start is above its end wraps past midnight.
func Quiet(r *store.AlertRule, now time.Time) bool {
	if r.QuietStart == nil || r.QuietEnd == nil || r.QuietTZ == nil {
		return false
	}
	loc, err := time.LoadLocation(*r.QuietTZ)
	if err != nil {
		return false
	}
	local := now.In(loc)
	minutes := local.Hour()*60 + local.Minute()
	start, end := *r.QuietStart, *r.QuietEnd
	if start <= end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}
