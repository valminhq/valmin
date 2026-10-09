package alerts

import (
	"encoding/json"
	"fmt"
	"slices"
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
	// HiddenFields names the message fields a rule leaves out, from the kind's MessageFields.
	// Empty shows every field.
	HiddenFields []string `json:"hidden_fields,omitempty"`
	// Timezone is the IANA zone a rule's messages show times in. Empty is UTC.
	Timezone string `json:"timezone,omitempty"`
}

// Message fields an event rule can hide.
const (
	FieldServerName = "server_name"
	FieldWorld      = "world"
	FieldJoinCode   = "join_code"
	FieldPort       = "port"
	FieldTime       = "time"
	FieldReason     = "reason"
)

// MessageFields is the fields a kind's message can carry, each of which a rule can hide.
func MessageFields(k Kind) []string {
	switch k {
	case KindServerStarted:
		return []string{FieldServerName, FieldWorld, FieldJoinCode, FieldPort, FieldTime}
	case KindServerStopped:
		return []string{FieldReason, FieldServerName, FieldWorld, FieldTime}
	}
	return nil
}

// Shows reports whether a rule's message carries field.
func (w ParamsWire) Shows(field string) bool { return !slices.Contains(w.HiddenFields, field) }

// Location is the zone a rule's messages show times in. An unknown zone reads as UTC, so a
// zone the host has since lost costs the local time, not the message.
func (w ParamsWire) Location() *time.Location {
	if w.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
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
