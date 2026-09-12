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

// TestSaveCompleteIsThePhaseFiveLiteral is B2 against the numbered save grammar. Every phase
// line ends in `done`, and phase 1 reports it before a byte is written, so only the phase
// counter separates the completion line from the announcement of an empty check.
func TestSaveCompleteIsThePhaseFiveLiteral(t *testing.T) {
	phases := []string{
		"### Save World Thread Started! ###",
		"Considering autobackup for World. World time: 92.97997, short time: 7200, " +
			"long time: 43200, backup count: 4",
		"Skipping backup. World session not long enough.",
		"World save (1/5) Cloud & Backup checks done [0ms] => Save number 1",
		"World save (2/5) Chunks writing done [77ms]",
		"World save (3/5) DB2 writing done [23ms]",
		"World save (4/5) FWL writing done [4ms]",
	}
	for _, line := range phases {
		if ev, ok := DefaultPatterns.Match(line); ok && ev.Kind == EventSaveComplete {
			t.Errorf("%q matched the save-complete pattern", line)
		}
	}
	complete := "World save (5/5) done. Total time [114ms]"
	if ev, ok := DefaultPatterns.Match(complete); !ok || ev.Kind != EventSaveComplete {
		t.Errorf("%q did not match save-complete: %v, %v", complete, ev.Kind, ok)
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
		{
			`Session "ese" with join code 793106 and IP 85.114.198.238:2476 is active with 0 player(s)`,
			EventCrossplaySession, "793106",
		},
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

// TestBlankJoinCodeIsNotAJoinCode is Q25's other half. The registration line logs the field
// empty, so a pattern loose enough to report `,` or `and` as the code would put a fake join
// code in front of an operator — worse than the blank the panel showed before.
func TestBlankJoinCodeIsNotAJoinCode(t *testing.T) {
	for _, line := range []string{
		`New session server "ese" that has join code , now 0 player(s)`,
		`Session "ese" with join code  and IP 85.114.198.238:2476 is active`,
	} {
		if ev, ok := DefaultPatterns.Match(line); ok && ev.Kind == EventCrossplaySession {
			t.Errorf("%q reported a join code of %q", line, ev.Groups[1])
		}
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

// unshippedLines are lines 03 §3.5 saw and the set deliberately does not adopt. They are
// written in pieces so this file's own text cannot satisfy the source scan below.
//
// `Closing socket` never appeared in the crossplay capture at all — it is presumably
// Steam-socket-only, and no Steam-socket session has been measured. `Got handshake from
// client` fired for the login that was rejected two seconds later, so it marks a socket, not
// a player.
var unshippedLines = []string{
	"Got hand" + "shake from client 76561198000000000",
	"Clos" + "ing socket 76561198000000000",
}

// TestUnshippedPlayerLinesStayUnshipped keeps the two measured-but-rejected lines out of the
// set. Either would report a player the server does not have.
func TestUnshippedPlayerLinesStayUnshipped(t *testing.T) {
	for _, l := range unshippedLines {
		if ev, ok := DefaultPatterns.Match(l); ok {
			t.Errorf("%q matched %v; it marks a socket, not a player (Q7)", l, ev.Kind)
		}
	}
	scanForPlayerPatterns(t)
}

// scanForPlayerPatterns is F2 and ADR-080 enforced rather than remembered: the daemon matches
// player lines in exactly one place. A second matcher — in the hub, in a job, in a handler —
// is a pattern nobody re-measures when the game moves, and its silent wrong answer is a
// player count that is merely stale rather than absent.
func scanForPlayerPatterns(t *testing.T) {
	t.Helper()
	forbidden := []string{
		"hand" + "shake", "Clos" + "ing socket", "RPC_" + "Disconnect",
		"play" + "er(s)", "play" + `er\(s\)`, "ZRpc " + "timeout",
	}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasPrefix(path, filepath.Join("..", "instance")+string(os.PathSeparator)) {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s matches %q; only the log reader parses the game's lines (F2)", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
