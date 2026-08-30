package store

import (
	"testing"
	"time"
)

// TestStoredTimeSortsChronologically is the regression test for the format itself.
// time.RFC3339Nano drops trailing zeros, so "…10:00:00Z" and "…10:00:00.5Z" compare in the
// wrong order as text — and SQLite compares these columns as text. Every expiry in the
// panel is a `>` against one of these values.
func TestStoredTimeSortsChronologically(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	ordered := []time.Time{
		base,
		base.Add(time.Nanosecond),
		base.Add(500 * time.Millisecond),
		base.Add(time.Second),
		base.Add(time.Minute),
		base.AddDate(0, 0, 1),
		base.AddDate(1, 0, 0),
	}

	for i := 1; i < len(ordered); i++ {
		earlier, later := FormatTime(ordered[i-1]), FormatTime(ordered[i])
		if earlier >= later {
			t.Errorf("%q is chronologically before %q but does not sort before it", earlier, later)
		}
	}
}

// TestStoredTimeComparesInSQLite runs the same claim through the driver, because the
// property that matters is SQLite's, not Go's: a TIMESTAMP column takes NUMERIC affinity,
// which a timestamp string does not satisfy, so it falls back to a text comparison.
func TestStoredTimeComparesInSQLite(t *testing.T) {
	db := open(t)
	if _, err := db.Writer.ExecContext(t.Context(),
		`CREATE TABLE ts_probe (label TEXT, at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	for label, at := range map[string]time.Time{
		"past":   base.Add(-time.Second),
		"exact":  base,
		"soon":   base.Add(500 * time.Millisecond),
		"future": base.Add(time.Hour),
	} {
		if _, err := db.Writer.ExecContext(t.Context(),
			`INSERT INTO ts_probe VALUES (?, ?)`, label, FormatTime(at)); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.Reader.QueryContext(t.Context(),
		`SELECT label FROM ts_probe WHERE at > ? ORDER BY at`, FormatTime(base))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatal(err)
		}
		got = append(got, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := []string{"soon", "future"}
	if len(got) != len(want) {
		t.Fatalf("at > base returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at > base returned %v, want %v", got, want)
		}
	}
}

func TestParseTimeRoundTrips(t *testing.T) {
	want := time.Date(2026, 8, 30, 10, 0, 0, 123456789, time.UTC)
	got, err := ParseTime(FormatTime(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("round trip gave %s, want %s", got, want)
	}

	// A value written in plain RFC3339 still loads, so a row edited by hand is readable.
	if _, err := ParseTime("2026-08-30T10:00:00Z"); err != nil {
		t.Errorf("plain RFC3339 did not parse: %v", err)
	}
	if _, err := ParseTime("not a time"); err == nil {
		t.Error("garbage parsed as a timestamp")
	}
}
