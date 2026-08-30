package instance

import (
	"strings"
	"unicode"
)

// PlayerList names one of 03 §4's three files. A closed set, because these three names are
// the game's, not the panel's — there is no fourth list and no user-chosen filename that
// could reach WriteWorldFile through this path.
type PlayerList string

const (
	AdminList     PlayerList = "adminlist.txt"
	BannedList    PlayerList = "bannedlist.txt"
	PermittedList PlayerList = "permittedlist.txt"
)

// PlayerIDRule names why an entry was rejected, without any HTTP or presentation concern —
// internal/api translates each into 11 §2.4's field-error shape.
type PlayerIDRule string

const (
	RuleIDHasWhitespace  PlayerIDRule = "id_has_whitespace"
	RuleIDNotPrintable   PlayerIDRule = "id_not_printable"
	RuleIDLooksCommented PlayerIDRule = "id_looks_commented"
)

// PlayerIDViolation is one rejected entry, carrying its index so the caller can point at
// the row the user actually typed.
type PlayerIDViolation struct {
	Index int
	ID    string
	Rule  PlayerIDRule
}

// ParsePlayerList reads one of the three files into its entries.
//
// `↯` It does not validate. 03 §4 warns against round-tripping a user's file through a
// parser that could reorder or annotate it, and a file the panel did not write may hold
// entries this build would refuse — reading is not the moment to lose them. Validation is
// NormalisePlayerIDs, on the way in.
func ParsePlayerList(data []byte) []string {
	ids := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			ids = append(ids, trimmed)
		}
	}
	return ids
}

// NormalisePlayerIDs prepares ids for writing, returning what may be written and what was
// refused. 03 §4's format is strict — one id per line, no comments, no trailing text —
// and the failure mode it warns about is silent: a stray character does not raise an error,
// the admin simply is not an admin. So an entry that cannot be written cleanly is refused
// loudly here instead.
//
// `↯` The form of an accepted id is preserved exactly, and that is deliberate. 03 §4 gives
// the forward-compatible shape as `[Platform]_[User ID]` but **never states the literal
// platform token** — `[Platform]` is the doc's own placeholder, and nothing in the pack
// measures it. Rewriting a working bare SteamID64 into a guessed `Steam_…` would, if the
// guess is wrong, silently strip an existing admin of admin: the precise failure 03 §4
// exists to prevent, inverted. Bare SteamID64 still works for Steam players (03 §4), so
// both forms are accepted and neither is rewritten. Tracked as Q30.
func NormalisePlayerIDs(ids []string) (clean []string, violations []PlayerIDViolation) {
	clean = []string{}
	for i, raw := range ids {
		id := strings.TrimSpace(raw)
		switch {
		case id == "":
			// A blank row is what an empty textarea line looks like, not a mistake worth
			// reporting. Dropped rather than refused.
			continue
		case strings.HasPrefix(id, "#") || strings.HasPrefix(id, "//"):
			// The file has no comment syntax, so a line that looks like one would be read
			// as an id and silently match nobody. Refusing is the only way the user finds
			// out they wrote a comment into a file that has none — and it is checked first
			// because a commented line almost always also has a space in it, and "no
			// comments here" is the answer that explains what actually went wrong.
			violations = append(violations, PlayerIDViolation{i, raw, RuleIDLooksCommented})
		case strings.ContainsFunc(id, unicode.IsSpace):
			violations = append(violations, PlayerIDViolation{i, raw, RuleIDHasWhitespace})
		case strings.ContainsFunc(id, notPrintable):
			violations = append(violations, PlayerIDViolation{i, raw, RuleIDNotPrintable})
		default:
			clean = append(clean, id)
		}
	}
	return clean, violations
}

func notPrintable(r rune) bool { return !unicode.IsPrint(r) }

// FormatPlayerList renders ids as the file's bytes: one per line, trailing newline, nothing
// else. No header, no comment, no ordering of the panel's own — 03 §4 is explicit that the
// panel must not annotate this file, and a "# managed by Valmin" line would break the first
// entry after it.
func FormatPlayerList(ids []string) []byte {
	if len(ids) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(ids, "\n") + "\n")
}
