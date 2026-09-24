package instance

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync/atomic"
)

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
	// EventPluginFailed is a plugin the chainloader refused or could not load, named in group 1
	// in the same `Name Version` form EventPluginLoading uses (Q38).
	EventPluginFailed EventKind = "plugin_failed"
	// EventCrossplayRegistered reports a successful PlayFab registration, which only
	// appears with -crossplay.
	EventCrossplayRegistered EventKind = "crossplay_registered"
	// EventCrossplaySession carries the join code of an active crossplay session in
	// group 1. Q25.
	EventCrossplaySession EventKind = "crossplay_session"
	// EventPlayerCount carries the server's own count of connected sockets in group 1. It is
	// a socket count, not a player count: it rises before the password is checked (Q7).
	EventPlayerCount EventKind = "player_count"
	// EventConnections carries the periodic authoritative connection count in group 1,
	// emitted every 600 s.
	EventConnections EventKind = "connections"
	// EventPeerJoined is the authenticated join. A rejected password never reaches it.
	EventPeerJoined EventKind = "peer_joined"
	// EventPeerLeft is a disconnect the client asked for or the server accepted.
	EventPeerLeft EventKind = "peer_left"
	// EventDiskThresholds carries the server's own disk floors: the space it saw in group 1,
	// the floor below which it silently stops saving in group 2, and the floor below which it
	// warns in group 3. 03 §3.4 requires these be read rather than hardcoded, because the
	// server computes them at runtime.
	EventDiskThresholds EventKind = "disk_thresholds"
	// EventPlayerIdentity is one entry of the server's own account history: a display name in
	// group 2 and a platform id in group 3, in the Steam_<id> form the player lists take
	// (03 §4). Group 4 is a second identifier nothing has identified, captured to anchor the
	// shape and not stored.
	EventPlayerIdentity EventKind = "player_identity"
	// EventPlatformID carries the platform id a connecting socket presented in group 2, and
	// the remote id it presented it over in group 1. It is a connection attempt and not a
	// session: the one measured at 08:29:22 was rejected two seconds later for a wrong
	// password.
	EventPlatformID EventKind = "platform_id"
	// EventPeerTimeout is a peer dropping without saying goodbye. It is the one ending that
	// emits no count line afterwards, which is why the count it leaves behind is unknowable
	// rather than decrementable (Q7).
	EventPeerTimeout EventKind = "peer_timeout"
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
// The five player patterns are a second capture, on l-0.221.12: one real client on a modded
// crossplay server, recorded in M5-PLAYER-EVIDENCE.md. They are stamped to that build like
// the rest, and no member of the set covers a vanilla Steam-socket session, which was not
// captured (ADR-154, Q7).
var DefaultPatterns = PatternSet{
	// The full literal, not a prefix: four save phases share `World save writing` and two
	// share the stem `finish`, so a loose pattern archives a half-written world (B2).
	{EventSaveComplete, regexp.MustCompile(`World save writing finished`)},
	// The numbered save grammar's completion line, anchored on the phase counter. Every phase
	// reports `done`, and phase 1 reports it before any world byte is written, so the counter
	// is the only thing separating completion from that announcement (B2, 03 §3.2.1).
	{EventSaveComplete, regexp.MustCompile(`World save \(5/5\) done`)},
	{EventReady, regexp.MustCompile(`Game server connected`)},
	{EventSaved, regexp.MustCompile(`Saved (\d+) ZDOs`)},
	{EventQuit, regexp.MustCompile(`Game - OnApplicationQuit`)},
	// One measured line, three numbers, anchored between its own literals. The server refuses
	// to save below the second and warns below the third, and below that floor it runs
	// normally and stops persisting the world silently (03 §3.4).
	{EventDiskThresholds, regexp.MustCompile(
		`Available space to current user: (\d+)\. ` +
			`Saving is blocked if below: (\d+) bytes\. ` +
			`Warnings are given if below: (\d+)`)},
	// The `?` is mandatory: one plugin logs "plugin", singular (E9).
	{EventPluginCount, regexp.MustCompile(`(\d+) plugins? to load`)},
	// Q38's failure lines. These three are BepInEx 5.4's chainloader messages as its source
	// writes them, not yet a capture from a failing server: a plugin whose load threw, one
	// refused over a missing or incompatible dependency, and one skipped because a plugin it
	// depends on was refused. `Skipping [...] because of process filters` is not among them: a
	// client-only plugin skipped on the dedicated server is working as intended. If a capture
	// disagrees, game.log_patterns overrides the kind until a release corrects it.
	//
	// Ahead of plugin_loading for safety, though neither literal matches the other: Match takes
	// the first hit, and a failure must never be counted as a load.
	{EventPluginFailed, regexp.MustCompile(`(?:Error loading|Could not load) \[([^\]]+)\]`)},
	{EventPluginFailed, regexp.MustCompile(
		`Skipping \[([^\]]+)\] because it has a dependency that was not loaded`)},
	{EventPluginLoading, regexp.MustCompile(`Loading \[([^\]]+)\]`)},
	{EventCrossplayRegistered, regexp.MustCompile(`Register PlayFab server`)},
	// The registration line's code is blank (03 §1.4); this one carries it. Anchored between
	// literals rather than on the session name, which may contain a quote (Q25).
	{EventCrossplaySession, regexp.MustCompile(`with join code (\S+) and IP `)},
	// After the crossplay session pattern, which claims the one line carrying both a join
	// code and a count. Match returns the first hit, so the order is the choice between them.
	{EventPlayerCount, regexp.MustCompile(`now (\d+) player\(s\)`)},
	{EventConnections, regexp.MustCompile(`Connections (\d+) ZDOS:`)},
	// No space after the comma. It is the literal the server prints.
	{EventPeerJoined, regexp.MustCompile(`Server: New peer connected,sending global keys`)},
	{EventPeerLeft, regexp.MustCompile(`RPC_Disconnect`)},
	// Two measured shapes that name an account. The display name is matched greedily, so a
	// name containing brackets keeps them and the identifiers are read from the last pair;
	// an entry with no name still matches, which matters because the id is the part an
	// operator needs (docs/evidence/player-identity-2026-09-14.md).
	{EventPlayerIdentity, regexp.MustCompile(
		`Player history entry with index (\d+): +(.*) \((\S+), (\S+)\)`)},
	{EventPlatformID, regexp.MustCompile(
		`socket with remote ID (\S+) received local Platform ID (\S+)`)},
	// `ZRpc timeout set to 90s` shares the stem and is not an ending.
	{EventPeerTimeout, regexp.MustCompile(`ZRpc timeout detected`)},
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

// WithOverrides returns a copy of the set with each named kind's patterns replaced by the
// operator's own (Q32). A game patch that rewords a line would otherwise leave the panel blind to
// it until a release measured the new literal; this is how an operator bridges that gap.
//
// An override replaces every pattern of its kind, in the position the first one held, because
// Match returns the first hit and the order encodes decisions (the crossplay session line before
// the player count). A kind with two measured grammars takes one regex covering whichever the
// operator's servers print, alternation included.
//
// Refused, with every problem reported at once: an unknown kind, a regex that does not compile,
// one that matches an empty line (it would match every line, and a save_complete that fires on
// anything archives a half-written world, B2), and one with fewer capture groups than the kind's
// consumers read.
func (ps PatternSet) WithOverrides(overrides map[string]string) (PatternSet, error) {
	if len(overrides) == 0 {
		return ps, nil
	}
	groups := map[EventKind]int{}
	for _, p := range ps {
		groups[p.Kind] = max(groups[p.Kind], p.Re.NumSubexp())
	}

	var errs []error
	compiled := make(map[EventKind]*regexp.Regexp, len(overrides))
	for _, name := range slices.Sorted(maps.Keys(overrides)) {
		kind := EventKind(name)
		want, known := groups[kind]
		if !known {
			errs = append(errs, fmt.Errorf("%s is not a log event the panel reads; known: %v",
				name, slices.Sorted(maps.Keys(groups))))
			continue
		}
		re, err := regexp.Compile(overrides[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if re.MatchString("") {
			errs = append(errs, fmt.Errorf("%s: %q matches an empty line, so it would match every line",
				name, overrides[name]))
			continue
		}
		if re.NumSubexp() < want {
			errs = append(errs, fmt.Errorf("%s: %q has %d capture groups; the panel reads %d",
				name, overrides[name], re.NumSubexp(), want))
			continue
		}
		compiled[kind] = re
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	out := make(PatternSet, 0, len(ps))
	placed := map[EventKind]bool{}
	for _, p := range ps {
		re, ok := compiled[p.Kind]
		if !ok {
			out = append(out, p)
			continue
		}
		if !placed[p.Kind] {
			out = append(out, Pattern{Kind: p.Kind, Re: re})
			placed[p.Kind] = true
		}
	}
	return out, nil
}

// active is the set every reader matches against. It is process-wide because the patterns
// describe the game build, not an instance, and is written once, at startup, before any reader
// exists.
var active atomic.Pointer[PatternSet]

// UsePatterns installs the set the daemon matches with, DefaultPatterns plus the operator's
// overrides.
func UsePatterns(ps PatternSet) { active.Store(&ps) }

// ActivePatterns is the set in force: the one UsePatterns installed, or DefaultPatterns.
func ActivePatterns() PatternSet {
	if ps := active.Load(); ps != nil {
		return *ps
	}
	return DefaultPatterns
}
