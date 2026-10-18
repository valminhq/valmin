package scan

import (
	"testing"
	"time"
)

// TestFiresBetweenFollowsTheSchedulesOwnDays asserts that a weekday backup schedule owes no run
// over the weekend after a Friday backup, and owes runs on the days it fires.
func TestFiresBetweenFollowsTheSchedulesOwnDays(t *testing.T) {
	kyiv, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Fatal(err)
	}
	friday := time.Date(2026, 10, 16, 3, 2, 0, 0, kyiv)
	cases := []struct {
		name string
		now  time.Time
		want int
	}{
		{"Sunday", time.Date(2026, 10, 18, 12, 0, 0, 0, kyiv), 0},
		{"Monday after its run", time.Date(2026, 10, 19, 3, 30, 0, 0, kyiv), 1},
		{"Tuesday after its run", time.Date(2026, 10, 20, 3, 30, 0, 0, kyiv), 2},
	}
	for _, tc := range cases {
		if got := firesBetween("0 3 * * 1-5", "Europe/Kyiv", friday, tc.now); got != tc.want {
			t.Errorf("%s: %d runs since Friday's backup, want %d", tc.name, got, tc.want)
		}
	}
}
