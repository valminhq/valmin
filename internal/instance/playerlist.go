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

// commentPrefix is the comment marker the game itself writes, measured against build 21981559,
// where all three files ship with one such header line (03 §4).
//
// `#` is deliberately not a second marker: nothing measured shows the game honouring it, and
// treating it as one would discard a line the server may be reading as an id.
const commentPrefix = "//"

// ParsePlayerList reads one of the three files into its entries, skipping comment lines. It
// does not validate: a file the panel did not write may hold entries this build would refuse,
// and reading is not the moment to lose them (03 §4). Validation is NormalisePlayerIDs, on the
// way in.
func ParsePlayerList(data []byte) []string {
	ids := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, commentPrefix) {
			ids = append(ids, trimmed)
		}
	}
	return ids
}

// PlayerListComments returns the comment lines of an existing file, so a rewrite can put them
// back. The game ships each of these files with a header line, and dropping it on the first save
// would lose bytes the panel did not write (03 §4, 11 §1.1).
func PlayerListComments(data []byte) []string {
	comments := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, commentPrefix) {
			comments = append(comments, trimmed)
		}
	}
	return comments
}

// NormalisePlayerIDs prepares ids for writing, returning what may be written and what was
// refused. The file's format is strict and its failure mode silent: a stray character raises no
// error, the admin simply is not an admin (03 §4), so an entry that cannot be written cleanly is
// refused here instead.
//
// An accepted id's form is preserved exactly. Both bare SteamID64 and the `[Platform]_[User ID]`
// shape work, and the literal platform token is unmeasured, so rewriting one into the other
// could silently strip an admin of admin (Q30).
func NormalisePlayerIDs(ids []string) (clean []string, violations []PlayerIDViolation) {
	clean = []string{}
	for i, raw := range ids {
		id := strings.TrimSpace(raw)
		switch {
		case id == "":
			// A blank row is what an empty textarea line looks like, not a mistake worth
			// reporting. Dropped rather than refused.
			continue
		case strings.HasPrefix(id, commentPrefix) || strings.HasPrefix(id, "#"):
			// A comment submitted as an entry is not a player id, and the file's own comments are
			// preserved separately. Checked before the space rule, which such a line also breaks
			// and which would explain the rejection less clearly.
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

// FormatPlayerList renders the file's bytes: the comments it already had, then one id per line,
// then a trailing newline. Nothing of the panel's own is added (03 §4).
//
// Entry order is preserved exactly and comments are emitted first. On every measured file the
// comments already lead, so this is byte-identical; an interleaved one is moved rather than
// lost.
func FormatPlayerList(comments, ids []string) []byte {
	lines := make([]string, 0, len(comments)+len(ids))
	lines = append(lines, comments...)
	lines = append(lines, ids...)
	if len(lines) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
