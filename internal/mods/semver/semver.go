// Package semver compares Thunderstore version strings — strict major.minor.patch
// (03 §6.2). Extracted from thunderstore.Package.Latest's private helpers,
// when the resolver's diamond resolution needed the identical comparison a second time.
package semver

import (
	"strconv"
	"strings"
)

// Parse decodes a strict major.minor.patch version string. ok is false for anything
// else — 03 §6.2's format is not always honoured in the wild, and this package does not
// guess at what a malformed version means.
func Parse(v string) ([3]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Greater reports whether a is a higher version than b. Both must already be parsed — the
// caller decides what an unparseable version means in its own context, which differs
// between thunderstore.Package.Latest (falls back to listing order) and the resolver
// (refuses to compare at all; see resolver.Resolve).
func Greater(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
