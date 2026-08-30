package instance

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// TestNormalisePlayerIDsTrimsRatherThanWritingAsIs is 03 §4's strict format: a stray
// character does not raise an error from the game, the admin simply is not an admin — so
// surrounding whitespace is stripped on the way in rather than committed.
func TestNormalisePlayerIDsTrimsRatherThanWritingAsIs(t *testing.T) {
	clean, violations := NormalisePlayerIDs([]string{
		"  76561198000000000  ",
		"\tSteam_76561198000000001\n",
		"",
		"   ",
	})
	if len(violations) != 0 {
		t.Fatalf("violations = %+v, want none", violations)
	}
	want := []string{"76561198000000000", "Steam_76561198000000001"}
	if !slices.Equal(clean, want) {
		t.Errorf("clean = %q, want %q", clean, want)
	}
}

// TestNormalisePlayerIDsRefusesWhatWouldSilentlyFail covers the entries 03 §4 warns break an
// account's access with no error anywhere.
func TestNormalisePlayerIDsRefusesWhatWouldSilentlyFail(t *testing.T) {
	tests := []struct {
		name string
		id   string
		rule PlayerIDRule
	}{
		{"internal space", "7656119 8000000000", RuleIDHasWhitespace},
		{"trailing comment", "76561198000000000 # my friend", RuleIDHasWhitespace},
		{"hash comment", "# the co-admins", RuleIDLooksCommented},
		{"slash comment", "// the co-admins", RuleIDLooksCommented},
		{"control character", "76561198000\x00000000", RuleIDNotPrintable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clean, violations := NormalisePlayerIDs([]string{tc.id})
			if len(clean) != 0 {
				t.Errorf("clean = %q, want nothing written", clean)
			}
			if len(violations) != 1 || violations[0].Rule != tc.rule {
				t.Fatalf("violations = %+v, want one %s", violations, tc.rule)
			}
			if violations[0].Index != 0 {
				t.Errorf("index = %d, want 0 so the UI can point at the row", violations[0].Index)
			}
		})
	}
}

// TestNormalisePlayerIDsPreservesTheFormTheUserWrote is the Q30 decision, given a test so a
// later "helpful" upgrade cannot land silently. 03 §4 names the forward-compatible form as
// `[Platform]_[User ID]` but never states the literal platform token — rewriting a working
// bare SteamID64 into a guessed prefix would silently strip an admin of admin, which is the
// exact failure 03 §4 exists to prevent.
func TestNormalisePlayerIDsPreservesTheFormTheUserWrote(t *testing.T) {
	ids := []string{"76561198000000000", "Steam_76561198000000001", "XboxLive_2814000000000000"}
	clean, violations := NormalisePlayerIDs(ids)
	if len(violations) != 0 {
		t.Fatalf("violations = %+v, want none — every prefixed form must be accepted", violations)
	}
	if !slices.Equal(clean, ids) {
		t.Errorf("clean = %q, want %q unchanged", clean, ids)
	}
}

// TestPlayerListRoundTrips is 03 §4's "never round-trip a user's file through a parser that
// could reorder or annotate it": order is preserved and nothing is added.
func TestPlayerListRoundTrips(t *testing.T) {
	original := []byte("Steam_3\n76561198000000000\nSteam_1\n")
	ids := ParsePlayerList(original)
	want := []string{"Steam_3", "76561198000000000", "Steam_1"}
	if !slices.Equal(ids, want) {
		t.Fatalf("parsed = %q, want %q in file order", ids, want)
	}
	if got := FormatPlayerList(PlayerListComments(original), ids); !bytes.Equal(got, original) {
		t.Errorf("round trip = %q, want %q byte-identical", got, original)
	}
}

// shippedAdminList is what the game itself writes into adminlist.txt — measured verbatim
// from a real install at build 21981559, two spaces after "ID" included. Its siblings
// bannedlist.txt and permittedlist.txt carry the same line with the word swapped.
const shippedAdminList = "// List admin players ID  ONE per line\n"

// TestPlayerListPreservesTheGamesOwnHeader is the regression for a bug this package shipped
// with: 03 §4 states the format has "no comments", so the first cut refused any line
// starting with "//" — which is every one of these files as the game ships them. An operator
// pressing Save on an untouched admin list got a 422, and had the write gone through it
// would have erased the header. 03 §4 has been corrected from the shipped file.
func TestPlayerListPreservesTheGamesOwnHeader(t *testing.T) {
	original := []byte(shippedAdminList)

	if ids := ParsePlayerList(original); len(ids) != 0 {
		t.Errorf("a freshly shipped list parsed as %q, want no entries", ids)
	}
	comments := PlayerListComments(original)
	if len(comments) != 1 || comments[0] != strings.TrimSpace(shippedAdminList) {
		t.Fatalf("comments = %q, want the shipped header", comments)
	}
	if got := FormatPlayerList(comments, nil); !bytes.Equal(got, original) {
		t.Errorf("an untouched list round-tripped to %q, want %q", got, original)
	}

	// Adding an admin keeps the header and appends below it.
	got := FormatPlayerList(comments, []string{"Steam_1"})
	want := shippedAdminList + "Steam_1\n"
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("after adding an admin = %q, want %q", got, want)
	}
}

// TestParsePlayerListDoesNotTreatHashAsAComment: only "//" is measured. Nothing shows the
// game honouring "#", and silently dropping such a line would discard an entry the server
// may well be reading.
func TestParsePlayerListDoesNotTreatHashAsAComment(t *testing.T) {
	ids := ParsePlayerList([]byte("# not a known comment marker\nSteam_1\n"))
	if len(ids) != 2 {
		t.Errorf("parsed = %q, want both lines kept", ids)
	}
	if c := PlayerListComments([]byte("# not a known comment marker\n")); len(c) != 0 {
		t.Errorf("comments = %q, want none: # is not a measured marker", c)
	}
}

// TestParsePlayerListDoesNotValidate: a file the panel did not write may hold entries this
// build would refuse on the way in, and reading is not the moment to lose them.
func TestParsePlayerListDoesNotValidate(t *testing.T) {
	ids := ParsePlayerList([]byte("# hand-written comment\n76561198000000000\n"))
	if len(ids) != 2 {
		t.Errorf("parsed = %q, want both lines: reading must not drop what it would refuse to write", ids)
	}
}

// TestFormatPlayerListOfNothingIsEmpty — not a lone newline, which reads as one blank entry.
func TestFormatPlayerListOfNothingIsEmpty(t *testing.T) {
	if got := FormatPlayerList(nil, nil); len(got) != 0 {
		t.Errorf("FormatPlayerList(nil, nil) = %q, want empty", got)
	}
}
