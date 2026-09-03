package store

import (
	"fmt"
	"time"
)

// TimeFormat is the on-disk layout for every TIMESTAMP column: RFC3339, UTC, and a
// fixed nine-digit fraction.
//
// The fixed width is the point. time.RFC3339Nano drops trailing zeros, so
// "…T10:00:00Z" and "…T10:00:00.5Z" compare in the wrong order as text — and SQLite gives
// a TIMESTAMP column NUMERIC affinity, which a timestamp string does not satisfy, so it is
// stored and compared as text. An expiry filtered with `expires_at > ?` would then never
// fire (D11, 09 §4). Measured against modernc.org/sqlite v1.57.0.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// FormatTime renders t for storage. Every write of a TIMESTAMP column goes through here.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// Now is the current instant as stored.
func Now() string { return FormatTime(time.Now()) }

// ParseTime reads a stored timestamp. It accepts any RFC3339 spelling, so a value written
// by hand or by a migration tool still loads.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}
