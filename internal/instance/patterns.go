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
