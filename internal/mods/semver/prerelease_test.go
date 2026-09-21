package semver

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Version
		ok   bool
	}{
		{"stable", "1.2.3", Version{Core: [3]int{1, 2, 3}}, true},
		{"beta", "2.0.14-beta.5", Version{Core: [3]int{2, 0, 14}, Pre: "beta.5"}, true},
		{"rc", "1.0.0-rc.1", Version{Core: [3]int{1, 0, 0}, Pre: "rc.1"}, true},
		{"hyphen inside the identifier", "1.0.0-alpha-1", Version{Core: [3]int{1, 0, 0}, Pre: "alpha-1"}, true},
		{"trailing hyphen, empty pre-release", "1.2.3-", Version{}, false},
		{"empty identifier", "1.2.3-beta..1", Version{}, false},
		{"non-ascii identifier", "1.2.3-β", Version{}, false},
		{"two-part core", "1.2-beta", Version{}, false},
		{"four-part core", "1.2.3.4", Version{}, false},
		{"empty", "", Version{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseVersion(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ParseVersion(%q) = (%+v, %v), want (%+v, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCompareOrdersTheWholeChain is semver.org's rule 11: a pre-release is lower than the
// same core without one, numeric identifiers compare as numbers and rank below alphanumeric
// ones, and a longer identifier list ranks above a prefix of itself.
func TestCompareOrdersTheWholeChain(t *testing.T) {
	ascending := []string{
		"0.9.9",
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"2.0.14-beta.5",
		"2.0.14",
	}
	parsed := make([]Version, len(ascending))
	for i, raw := range ascending {
		v, ok := ParseVersion(raw)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", raw)
		}
		parsed[i] = v
	}
	for i := range parsed {
		for j := range parsed {
			got := Compare(parsed[i], parsed[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if (got < 0) != (want < 0) || (got > 0) != (want > 0) {
				t.Errorf("Compare(%s, %s) = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
}

// TestParseStaysStrict asserts the original Parse is untouched: callers that reject a
// pre-release outright keep doing so.
func TestParseStaysStrict(t *testing.T) {
	if _, ok := Parse("2.0.14-beta.5"); ok {
		t.Error("Parse accepted a pre-release, which would change what every existing caller means")
	}
}
