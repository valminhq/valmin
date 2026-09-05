package instance

import "regexp"

// EventKind names a matched log line. Consumers ask for a kind rather than carrying a regex
// of their own, so the whole panel matches one set (14 §4.5).
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
	// EventCrossplaySession carries the join code of an active crossplay session in
	// group 1. Q25.
	EventCrossplaySession EventKind = "crossplay_session"
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

// gameTimestamp is 03 §3.5's optional prefix, carried by the networking subsystem's lines and
// not by the Unity Debug.Log ones. Stripping it before matching lets both grammars share one
// pattern set (E4). It is discarded, never parsed: it is locale-ambiguous and carries no
// timezone, and Docker's own timestamps are already correct (14 §4.1).
var gameTimestamp = regexp.MustCompile(`^\d{2}/\d{2}/\d{4} \d{2}:\d{2}:\d{2}: `)

// DefaultPatterns is the measured set from 04 §4, captured on pre-1.0 build 21981559. 03 §10
// expects the literals to move at 1.0; the response to a mismatch is to measure again, never
// to guess a replacement (CLAUDE.md §9).
//
// There is deliberately no join, leave or player-count pattern: stats.players stays null
// until Q7 is measured (E7).
var DefaultPatterns = PatternSet{
	// The full literal, not a prefix: four save phases share `World save writing` and two
	// share the stem `finish`, so a loose pattern archives a half-written world (B2).
	{EventSaveComplete, regexp.MustCompile(`World save writing finished`)},
	{EventReady, regexp.MustCompile(`Game server connected`)},
	{EventSaved, regexp.MustCompile(`Saved (\d+) ZDOs`)},
	{EventQuit, regexp.MustCompile(`Game - OnApplicationQuit`)},
	// The `?` is mandatory: one plugin logs "plugin", singular (E9).
	{EventPluginCount, regexp.MustCompile(`(\d+) plugins? to load`)},
	{EventPluginLoading, regexp.MustCompile(`Loading \[([^\]]+)\]`)},
	{EventCrossplayRegistered, regexp.MustCompile(`Register PlayFab server`)},
	// The registration line's code is blank (03 §1.4); this one carries it. Anchored between
	// literals rather than on the session name, which may contain a quote (Q25).
	{EventCrossplaySession, regexp.MustCompile(`with join code (\S+) and IP `)},
}

// PatternSet is the ordered set the reader matches every line against.
type PatternSet []Pattern

// Match reports the first pattern raw matches, with the game's timestamp prefix stripped.
// The patterns are substring searches: BepInEx lines carry a `[Info   :   BepInEx]` prefix of
// variable padding, so a start-anchored pattern would miss them (03 §5.3).
func (ps PatternSet) Match(raw string) (LogEvent, bool) {
	line := gameTimestamp.ReplaceAllLiteralString(raw, "")
	for _, p := range ps {
		if m := p.Re.FindStringSubmatch(line); m != nil {
			return LogEvent{Kind: p.Kind, Line: line, Groups: m}, true
		}
	}
	return LogEvent{}, false
}
