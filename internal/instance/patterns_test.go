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
		{
			"Available space to current user: 161039331328. " +
				"Saving is blocked if below: 6665246 bytes. " +
				"Warnings are given if below: 13330492",
			EventDiskThresholds, "161039331328",
		},
		{
			"Player history entry with index 0:  \u8449\u5ca9\u8317 (Steam_76561198165407024, 6754E9E0F16375A4)",
			EventPlayerIdentity, "0",
		},
		{
			"PlayFab socket with remote ID playfab/BFB3B9AADC0CDC42 " +
				"received local Platform ID Steam_76561198165407024",
			EventPlatformID, "playfab/BFB3B9AADC0CDC42",
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

// TestTheHistoryEntryYieldsTheIDEvenWithoutAName. The identifier is the part an operator
// needs, and a display name is not guaranteed to be there, so the pattern must not make one
// a condition of reading the other. A name carrying brackets is read whole, because the
// identifiers are taken from the last bracketed pair.
func TestTheHistoryEntryYieldsTheIDEvenWithoutAName(t *testing.T) {
	for _, tt := range []struct{ line, name, id string }{
		{
			"Player history entry with index 0:  \u8449\u5ca9\u8317 (Steam_76561198165407024, 6754E9E0F16375A4)",
			"\u8449\u5ca9\u8317", "Steam_76561198165407024",
		},
		{
			"Player history entry with index 3:  (Steam_76561190000000000, 6754E9E0F16375A4)",
			"", "Steam_76561190000000000",
		},
		{
			"Player history entry with index 1:  Bob (the builder) (Steam_76561190000000001, AAAA)",
			"Bob (the builder)", "Steam_76561190000000001",
		},
	} {
		ev, ok := DefaultPatterns.Match(tt.line)
		if !ok || ev.Kind != EventPlayerIdentity {
			t.Fatalf("Match(%q) = %v, %v", tt.line, ev.Kind, ok)
		}
		if ev.Groups[2] != tt.name {
			t.Errorf("name = %q, want %q", ev.Groups[2], tt.name)
		}
		if ev.Groups[3] != tt.id {
			t.Errorf("id = %q, want %q", ev.Groups[3], tt.id)
		}
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

// The disk line carries three numbers and all three are read: the floor below which the server
// stops persisting the world is the one the panel's alarm has to stay above, and it is computed
// at runtime rather than fixed, which is why it is read instead of hardcoded (03 §3.4).
func TestTheDiskLineCarriesAllThreeFloors(t *testing.T) {
	const line = "Available space to current user: 161039331328. " +
		"Saving is blocked if below: 6665246 bytes. " +
		"Warnings are given if below: 13330492"

	ev, ok := DefaultPatterns.Match(line)
	if !ok || ev.Kind != EventDiskThresholds {
		t.Fatalf("Match = %v, %v; want %v", ev.Kind, ok, EventDiskThresholds)
	}
	for i, want := range []string{"161039331328", "6665246", "13330492"} {
		if got := ev.Groups[i+1]; got != want {
			t.Errorf("group %d = %q, want %q", i+1, got, want)
		}
	}
}

// TestAnOverrideReplacesItsKindInPlace is Q32: an operator's pattern stands in for every
// built-in one of its kind, at the position the first held, and leaves the others alone.
func TestAnOverrideReplacesItsKindInPlace(t *testing.T) {
	ps, err := DefaultPatterns.WithOverrides(map[string]string{
		"save_complete": `World save (committed|\(6/6\) done)`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != len(DefaultPatterns)-1 {
		t.Fatalf("set has %d patterns, want the two save grammars folded into one", len(ps))
	}
	if ps[0].Kind != EventSaveComplete {
		t.Errorf("first pattern is %s, want the override where save_complete was", ps[0].Kind)
	}
	for line, want := range map[string]bool{
		"World save committed":        true,
		"World save (6/6) done":       true,
		"World save writing finished": false,
		"World save (5/5) done":       false,
	} {
		ev, ok := ps.Match(line)
		if got := ok && ev.Kind == EventSaveComplete; got != want {
			t.Errorf("%q matched save_complete = %v, want %v", line, got, want)
		}
	}
	if ev, ok := ps.Match("Game server connected"); !ok || ev.Kind != EventReady {
		t.Error("an override of one kind changed another")
	}
	if ev, ok := DefaultPatterns.Match("World save writing finished"); !ok || ev.Kind != EventSaveComplete {
		t.Error("WithOverrides modified the set it was called on")
	}
}

// TestUnsafeOverridesAreRefused asserts the refusals, each of which would otherwise run: a
// pattern matching everything makes save_complete fire mid-write (B2), and one short of a
// capture group makes its consumer index past the end.
func TestUnsafeOverridesAreRefused(t *testing.T) {
	for name, override := range map[string]map[string]string{
		"unknown kind":       {"world_ready": "ready"},
		"does not compile":   {"ready": "Game server ("},
		"matches every line": {"save_complete": "(World save)?"},
		"missing a group":    {"saved_zdos": `Saved \d+ ZDOs`},
		"one of several bad": {"ready": "Game server connected", "quit": ".*"},
		"empty string":       {"peer_left": ""},
		// B2 in the two shapes an operator writes by hand: the shared prefix, and the
		// numbered grammar's `done` without its counter. Both fire before the world is
		// written, and both pass every other check.
		"the save prefix":     {"save_complete": "World save"},
		"any numbered phase":  {"save_complete": `World save \(\d/5\).*done`},
		"the finish stem":     {"save_complete": "World save writing finish"},
		"a loose alternation": {"save_complete": `World save (writing finish\w+|\(\d/5\) done)`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DefaultPatterns.WithOverrides(override); err == nil {
				t.Errorf("WithOverrides(%v) accepted it", override)
			}
		})
	}
}

// TestNoOverridesIsTheMeasuredSet guards the default path every panel without an override
// takes.
func TestNoOverridesIsTheMeasuredSet(t *testing.T) {
	ps, err := DefaultPatterns.WithOverrides(nil)
	if err != nil || len(ps) != len(DefaultPatterns) {
		t.Fatalf("WithOverrides(nil) = %d patterns, %v", len(ps), err)
	}
	if got := ActivePatterns(); len(got) != len(DefaultPatterns) {
		t.Errorf("ActivePatterns before UsePatterns = %d patterns, want the measured set", len(got))
	}
}
