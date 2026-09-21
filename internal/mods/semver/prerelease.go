package semver

import (
	"strconv"
	"strings"
)

// Version is a version that may carry a pre-release suffix, as registries publish them
// (03 §6.1). Core is the strict major.minor.patch; Pre is everything after the first "-",
// empty for a stable release.
type Version struct {
	Core [3]int
	Pre  string
}

// ParseVersion decodes major.minor.patch, optionally followed by "-" and a pre-release of
// dot-separated identifiers drawn from [0-9A-Za-z-]. ok is false for anything else: an empty
// identifier, a character outside that set, or a core that Parse would reject.
func ParseVersion(v string) (Version, bool) {
	core, pre, hasPre := strings.Cut(v, "-")
	parsed, ok := Parse(core)
	if !ok {
		return Version{}, false
	}
	if !hasPre {
		return Version{Core: parsed}, true
	}
	if !validPreRelease(pre) {
		return Version{}, false
	}
	return Version{Core: parsed, Pre: pre}, true
}

// IsPreRelease reports whether this version carries a pre-release suffix.
func (v Version) IsPreRelease() bool { return v.Pre != "" }

// Compare orders two versions: negative when a is lower, zero when they are equal, positive
// when a is higher. The core decides first; on equal cores a version carrying a pre-release
// is lower than the same core without one, so 2.0.14-beta.5 sits below 2.0.14.
func Compare(a, b Version) int {
	for i := range a.Core {
		if a.Core[i] != b.Core[i] {
			if a.Core[i] > b.Core[i] {
				return 1
			}
			return -1
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePreRelease(a.Pre, b.Pre)
}

// comparePreRelease orders two pre-release suffixes identifier by identifier: numeric
// identifiers compare as numbers and rank below alphanumeric ones, and a longer list ranks
// above a shorter one it otherwise matches — so beta.11 is above beta.2, and rc above rc.1
// is false.
func comparePreRelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range min(len(as), len(bs)) {
		if as[i] == bs[i] {
			continue
		}
		an, aNum := numericIdentifier(as[i])
		bn, bNum := numericIdentifier(bs[i])
		switch {
		case aNum && bNum:
			if an > bn {
				return 1
			}
			return -1
		case aNum:
			return -1
		case bNum:
			return 1
		case as[i] > bs[i]:
			return 1
		default:
			return -1
		}
	}
	switch {
	case len(as) == len(bs):
		return 0
	case len(as) > len(bs):
		return 1
	default:
		return -1
	}
}

// numericIdentifier reports whether one identifier is all digits, and its value. A leading
// zero is still numeric here: the registries are not strict about it, and reading "01" as a
// string would sort it against numbers it plainly belongs beside.
func numericIdentifier(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || s == "" {
		return 0, false
	}
	return n, true
}

// validPreRelease reports whether every dot-separated identifier is non-empty and drawn from
// [0-9A-Za-z-].
func validPreRelease(pre string) bool {
	for id := range strings.SplitSeq(pre, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r == '-':
			default:
				return false
			}
		}
	}
	return true
}
