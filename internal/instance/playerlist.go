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

// commentPrefix is the comment marker the game itself writes. `↯` Measured, not assumed:
// against build 21981559 all three files ship containing exactly one line —
// `// List admin players ID  ONE per line` and its two siblings. 03 §4 states the format has
// "no comments"; the shipped file is a primary source and disagrees, so 03 §4 is corrected
// rather than the file being treated as malformed (31 Aug 2026).
//
// `#` is deliberately **not** a second marker. Nothing measured shows the game honouring it,
// and inventing one would mean silently discarding a line that the server may well be
// reading as an id.
const commentPrefix = "//"

// ParsePlayerList reads one of the three files into its entries, skipping comment lines.
//
// `↯` It does not validate. 03 §4 warns against round-tripping a user's file through a
// parser that could reorder or annotate it, and a file the panel did not write may hold
// entries this build would refuse — reading is not the moment to lose them. Validation is
// NormalisePlayerIDs, on the way in.
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

// PlayerListComments returns the comment lines of an existing file, so a rewrite can put
// them back.
//
// `↯` The game ships every one of these files with a header line, so a panel that dropped
// comments would erase it on the operator's first save — losing bytes the game wrote is the
// same failure as losing bytes a human typed (03 §4, 11 §1.1). Preserving is also the answer
// that is correct whichever way the server's own parser treats such a line: if it skips
// them the header is documentation, and if it reads them it is an id that matches nobody.
// Either way it was there before the panel arrived and it is not the panel's to remove.
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
		case strings.HasPrefix(id, commentPrefix) || strings.HasPrefix(id, "#"):
			// A comment submitted as an *entry* is still not a player id. The file's own
			// comments are preserved separately (PlayerListComments), so there is never a
			// reason to type one here — which makes refusing the clearer answer than
			// quietly writing a line that matches nobody. Checked first because such a line
			// almost always also contains a space, and "that is a comment" explains what
			// went wrong where "no spaces allowed" does not.
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

// FormatPlayerList renders the file's bytes: the comments it already had, then one id per
// line, then a trailing newline. Nothing of the panel's own is added — 03 §4 is explicit
// that the panel must not annotate these files, and a "managed by Valmin" line would be the
// panel doing exactly what it is warning others against.
//
// `↯` Entry order is preserved exactly; comments are emitted first. On every file measured
// the comments are already leading, so this is byte-identical in the real case, and a
// comment a user interleaved is moved rather than lost — the direction of error 03 §4 asks
// for.
func FormatPlayerList(comments, ids []string) []byte {
	lines := make([]string, 0, len(comments)+len(ids))
	lines = append(lines, comments...)
	lines = append(lines, ids...)
	if len(lines) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
