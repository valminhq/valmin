package api

import (
	"encoding/json"
	"fmt"
)

// paramsWire is the JSON form of a rule's thresholds. Durations are seconds with the unit in
// the field name (11 §1); the evaluator's own struct keeps them typed.
type paramsWire struct {
	CrashCount         int     `json:"crash_count,omitempty"`
	CrashWindowSeconds int     `json:"crash_window_seconds,omitempty"`
	StuckAfterSeconds  int     `json:"stuck_after_seconds,omitempty"`
	StaleFactor        float64 `json:"stale_factor,omitempty"`
}

// paramsWireOf reads a rule's stored thresholds back into their wire form.
func paramsWireOf(raw string) paramsWire {
	var w paramsWire
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &w); err != nil {
			return paramsWire{}
		}
	}
	return w
}

func encodeParams(w paramsWire) (string, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encode alert thresholds: %w", err)
	}
	return string(raw), nil
}
