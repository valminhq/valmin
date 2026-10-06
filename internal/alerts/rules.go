package alerts

import (
	"encoding/json"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

type ruleParamsWire struct {
	CrashCount         int     `json:"crash_count,omitempty"`
	CrashWindowSeconds int     `json:"crash_window_seconds,omitempty"`
	StuckAfterSeconds  int     `json:"stuck_after_seconds,omitempty"`
	StaleFactor        float64 `json:"stale_factor,omitempty"`
}

// decodeParams reads a rule's stored thresholds. Unreadable JSON falls back to the defaults
// rather than failing the scan: one bad rule must not stop every condition being evaluated.
func DecodeParams(raw string) Params {
	var w ruleParamsWire
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return Params{}
		}
	}
	return Params{
		CrashCount:  w.CrashCount,
		CrashWindow: time.Duration(w.CrashWindowSeconds) * time.Second,
		StuckAfter:  time.Duration(w.StuckAfterSeconds) * time.Second,
		StaleFactor: w.StaleFactor,
	}
}

// thresholds resolves a kind's params for one instance. The most specific enabled rule wins, so
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

// matches reports whether a rule covers this condition. A rule with no instance covers every
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
