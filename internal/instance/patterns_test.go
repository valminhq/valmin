package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatternsMatchBothLogGrammars is E4: the networking subsystem prefixes its lines with a
// game timestamp and the Unity Debug.Log path does not, so the same pattern has to match
// both forms. A start-anchored regex would pass the second case and silently fail every
// readiness line in production.
func TestPatternsMatchBothLogGrammars(t *testing.T) {
	for _, raw := range []string{
		"Game server connected",
		"08/20/2026 08:17:58: Game server connected",
	} {
		ev, ok := DefaultPatterns.Match(raw)
		if !ok || ev.Kind != EventReady {
			t.Fatalf("Match(%q) = %v, %v; want the readiness event", raw, ev.Kind, ok)
		}
		if strings.HasPrefix(ev.Line, "08/") {
			t.Errorf("Match(%q) reported the line with its timestamp prefix intact: %q", raw, ev.Line)
		}
	}
}

// TestSaveCompleteIsTheFullLiteral is B2, and it is the single most damaging pattern in the
// set to get loose. Four save phases share the prefix `World save writing` and two share the
// stem `finish`; a pattern that fires on `finishing` archives a half-written world.
func TestSaveCompleteIsTheFullLiteral(t *testing.T) {
	shutdown := []string{
		"World save writing starting",
		"World save writing started",
		"Saved 21771 ZDOs",
		"World save writing finishing",
	}
	for _, line := range shutdown {
		if ev, ok := DefaultPatterns.Match(line); ok && ev.Kind == EventSaveComplete {
			t.Errorf("%q matched the save-complete pattern", line)
		}
	}
	if ev, ok := DefaultPatterns.Match("World save writing finished"); !ok || ev.Kind != EventSaveComplete {
		t.Errorf("the save-complete line did not match: %v, %v", ev.Kind, ok)
	}
}

func TestPatternsMatchTheMeasuredLines(t *testing.T) {
	tests := []struct {
		line  string
		kind  EventKind
		group string
	}{
		// E9: one plugin logs "plugin", singular. The symptom of dropping the `?` is a
		// blank mods-loaded indicator with no error at all.
		{"[Info   :   BepInEx] 1 plugin to load", EventPluginCount, "1"},
		{"[Info   :   BepInEx] 0 plugins to load", EventPluginCount, "0"},
		{"[Info   :   BepInEx] Loading [Jotunn 2.29.2]", EventPluginLoading, "Jotunn 2.29.2"},
		{"Saved 21771 ZDOs", EventSaved, "21771"},
		{"Game - OnApplicationQuit", EventQuit, ""},
		{"Register PlayFab server", EventCrossplayRegistered, ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			ev, ok := DefaultPatterns.Match(tt.line)
			if !ok || ev.Kind != tt.kind {
				t.Fatalf("Match(%q) = %v, %v; want %v", tt.line, ev.Kind, ok, tt.kind)
			}
			if tt.group != "" && (len(ev.Groups) < 2 || ev.Groups[1] != tt.group) {
				t.Errorf("Match(%q) captured %v, want %q", tt.line, ev.Groups, tt.group)
			}
		})
	}
}

// TestBepInExPaddingIsNotMatched is 03 §5.3's warning that the whitespace inside
// `[Message:   BepInEx]` is variable — a pattern that encodes a fixed run of spaces works on
// the captured line and fails on the next build.
func TestBepInExPaddingIsNotMatched(t *testing.T) {
	if _, ok := DefaultPatterns.Match("[Info: BepInEx] 3 plugins to load"); !ok {
		t.Error("a line with different BepInEx padding did not match")
	}
}

// playerLines are the join, leave and count lines 03 §3.5 measured and deliberately did not
// adopt. They are written here in pieces so this file's own text cannot satisfy the source
// scan below.
var playerLines = []string{
	"Got hand" + "shake from client 76561198000000000",
	"Clos" + "ing socket 76561198000000000",
	// Without the registration text it sits on: 03 §3.5 saw the count on the same line
	// as `Register PlayFab server`, which the set matches on purpose, so a test using the
	// whole line would be satisfied by the wrong pattern and prove nothing.
	"ZNet: ... now 0 play" + "er(s)",
}

// TestNoPlayerCountPatternExists is E7 and Q7, enforced rather than remembered. Player
// counting is deliberately post-1.0: stats.players is null, and shipping a hardcoded pattern
// that silently reports 0 players forever — no error, no gap, just a wrong number — is the
// failure this test exists to prevent.
func TestNoPlayerCountPatternExists(t *testing.T) {
	for _, l := range playerLines {
		ev, ok := DefaultPatterns.Match(l)
		// The count line is the crossplay registration line, which the set does match —
		// on the registration, not on the count. Anything else matching is a player pattern.
		if ok && ev.Kind != EventCrossplayRegistered {
			t.Errorf("%q matched %v: Q7 is post-1.0 and stats.players stays null (E7)", l, ev.Kind)
		}
		if ok && len(ev.Groups) > 1 {
			t.Errorf("%q was matched with a capture group %v — that is a player count", l, ev.Groups)
		}
	}
	scanForPlayerPatterns(t)
}

// scanForPlayerPatterns catches the same mistake made outside this pattern set — a stats
// sampler or a job that matches join and leave lines of its own.
func scanForPlayerPatterns(t *testing.T) {
	t.Helper()
	forbidden := []string{
		"hand" + "shake", "Clos" + "ing socket", "RPC_" + "Disconnect",
		"play" + "er(s)", "play" + `er\(s\)`,
	}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "patterns_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s mentions %q: Q7 is post-1.0 and stats.players stays null (E7)", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
