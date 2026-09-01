package semver

import "testing"

func TestParseAcceptsStrictThreePart(t *testing.T) {
	got, ok := Parse("5.4.2333")
	if !ok || got != [3]int{5, 4, 2333} {
		t.Errorf("Parse(5.4.2333) = %v, %v", got, ok)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, v := range []string{"", "1.2", "1.2.3.4", "1.a.3", "v1.2.3", "1.2.-3"} {
		if _, ok := Parse(v); ok {
			t.Errorf("Parse(%q) ok = true, want false", v)
		}
	}
}

// TestGreaterComparesNumericallyNotLexically is the whole reason a string-sort ordering
// would be wrong: "1.10.0" sorts before "1.9.9" and "1.2.0" as plain text.
func TestGreaterComparesNumericallyNotLexically(t *testing.T) {
	a, _ := Parse("1.10.0")
	b, _ := Parse("1.9.9")
	if !Greater(a, b) {
		t.Error("Greater(1.10.0, 1.9.9) = false, want true")
	}
	if Greater(b, a) {
		t.Error("Greater(1.9.9, 1.10.0) = true, want false")
	}
}

func TestGreaterOfEqualVersionsIsFalse(t *testing.T) {
	a, _ := Parse("2.29.2")
	b, _ := Parse("2.29.2")
	if Greater(a, b) {
		t.Error("Greater(2.29.2, 2.29.2) = true, want false")
	}
}
