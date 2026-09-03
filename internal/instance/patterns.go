package instance

import "regexp"

// EventKind names a matched log line. Every pattern the panel matches has one, and the
// consumers ask for a kind rather than carrying a regex of their own (14 §4.5): the backup
// quiesce, the command-channel probe, the mods-loaded indicator and readiness are four
// callers of one set, so 9 September is one edit in one file.
type EventKind string

const (
	// EventReady is 12 §3.3's readiness anchor, measured in both vanilla and crossplay.
	EventReady EventKind = "ready"
	// EventSaveComplete is the line 02 §4.4's quiesce and 07 §4's probe block on.
	EventSaveComplete EventKind = "save_complete"
	// EventSaved carries the object count the save just wrote.
	EventSaved EventKind = "saved_zdos"
	// EventQuit marks the start of the graceful shutdown path.
	EventQuit EventKind = "quit"
	// EventPluginCount is BepInEx's chainloader count line.
	EventPluginCount EventKind = "plugin_count"
	// EventPluginLoading is one plugin being loaded. 03 §5.3 prefers counting these over
	// trusting EventPluginCount.
	EventPluginLoading EventKind = "plugin_loading"
	// EventCrossplayRegistered reports a successful PlayFab registration, which only
	// appears with -crossplay.
	EventCrossplayRegistered EventKind = "crossplay_registered"
)

// LogEvent is one matched line.
type LogEvent struct {
	Kind EventKind
	// Line is the line with the game's timestamp prefix already stripped.
	Line string
	// Groups are the pattern's submatches, index 0 being the whole match.
	Groups []string
}

// Pattern is one entry in the set.
type Pattern struct {
	Kind EventKind
	Re   *regexp.Regexp
}

// gameTimestamp is 03 §3.5's optional prefix. Two log grammars share the stream: the
// networking subsystem prefixes its lines with a game-emitted timestamp, the Unity
// Debug.Log path does not. Stripping it before matching is what keeps the two grammars from
// needing two pattern sets (E4).
//
// The stripped timestamp is discarded, never parsed: it is MM/DD/YYYY, carries no
// timezone and is locale-ambiguous. Docker's timestamps are already requested and already
// correct (14 §4.1).
var gameTimestamp = regexp.MustCompile(`^\d{2}/\d{2}/\d{4} \d{2}:\d{2}:\d{2}: `)

// DefaultPatterns is the measured set from 04 §4, stamped against pre-1.0 build 21981559
// (03's measurement banner).
//
// Every literal here was captured on a pre-1.0 build. Valheim 1.0 lands
// 9 September 2026 and 03 §10 expects log strings to move; they are re-measured
// them. Until it runs, these are measured-but-stale, and the correct response to a mismatch
// after 1.0 is to measure again — never to guess a replacement (CLAUDE.md §9).
//
// No join, leave or player-count pattern appears here, deliberately. Q7 is post-1.0 and
// stats.players is null (E7): a hardcoded pattern that silently reports 0 players forever is
// worse than no answer.
var DefaultPatterns = PatternSet{
	// The full literal, not a prefix. Four save phases share the prefix
	// `World save writing` and two share the stem `finish` — a loose pattern fires on
	// `finishing` and archives a half-written world (B2, 03 §3.2.1).
	{EventSaveComplete, regexp.MustCompile(`World save writing finished`)},
	{EventReady, regexp.MustCompile(`Game server connected`)},
	{EventSaved, regexp.MustCompile(`Saved (\d+) ZDOs`)},
	{EventQuit, regexp.MustCompile(`Game - OnApplicationQuit`)},
	// The `?` is mandatory (E9): one plugin logs "plugin", singular, and the symptom of
	// getting it wrong is a blank mods-loaded indicator with no error at all.
	{EventPluginCount, regexp.MustCompile(`(\d+) plugins? to load`)},
	{EventPluginLoading, regexp.MustCompile(`Loading \[([^\]]+)\]`)},
	{EventCrossplayRegistered, regexp.MustCompile(`Register PlayFab server`)},
}

// PatternSet is the ordered set the reader matches every line against.
type PatternSet []Pattern

// Match reports the first pattern raw matches, with the game's timestamp prefix stripped.
//
// The patterns are substring searches, not start-anchored ones. A start-anchored regex
// silently misses every networking line — readiness included — because those carry the
// prefix and the Unity ones do not (03 §3.5). Anchoring would also break the BepInEx lines,
// which carry a `[Info   :   BepInEx]` prefix of their own whose internal padding is
// variable (03 §5.3).
func (ps PatternSet) Match(raw string) (LogEvent, bool) {
	line := gameTimestamp.ReplaceAllLiteralString(raw, "")
	for _, p := range ps {
		if m := p.Re.FindStringSubmatch(line); m != nil {
			return LogEvent{Kind: p.Kind, Line: line, Groups: m}, true
		}
	}
	return LogEvent{}, false
}
