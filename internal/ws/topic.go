package ws

import (
	"strings"

	"github.com/valminhq/valmin/internal/authz"
)

// Kind is which of 14 §2.1's four topics a Topic names.
type Kind int

// The v1 registry, whole. Additions are cheap; a wildcard is not (ADR-040).
const (
	KindConsole Kind = iota + 1
	KindStats
	KindState
	KindJob
)

// Class is a topic's backpressure contract (ADR-039, 14 §5). It is not decorative: on a
// full queue a Lossy topic drops its oldest message and reports the gap, and a Lossless
// one closes the connection rather than let the client believe it saw everything.
type Class int

const (
	Lossy Class = iota
	Lossless
)

// Topic is a subscription target, parsed once at the edge and passed around as a value —
// the same rule as authz actions, job kinds and error codes. A string that reaches the
// fan-out is a string that can name a topic the caller was never authorized for.
type Topic struct {
	kind Kind
	id   string
}

// Kind reports which topic this is.
func (t Topic) Kind() Kind { return t.kind }

// ID is the instance id, or the job id for KindJob.
func (t Topic) ID() string { return t.id }

// String returns the wire form.
func (t Topic) String() string {
	switch t.kind {
	case KindConsole:
		return "instance." + t.id + ".console"
	case KindStats:
		return "instance." + t.id + ".stats"
	case KindState:
		return "instance." + t.id + ".state"
	case KindJob:
		return "job." + t.id
	default:
		return ""
	}
}

// Class is the topic's backpressure class.
//
// The asymmetry is the point (14 §5): drop bytes, never drop meaning. A console with a
// marked gap is still useful; a client that silently misses `state: stopped` shows a
// running server that is not, and the operator acts on it.
func (t Topic) Class() Class {
	switch t.kind {
	case KindState, KindJob:
		return Lossless
	case KindConsole, KindStats:
		return Lossy
	default:
		return Lossy
	}
}

// Action is the authorization this topic requires (09 §4.1). KindJob answers the zero
// Action: the topic string carries no instance, so the hub resolves the job row first and
// then authorizes InstanceView against whatever instance it names.
func (t Topic) Action() authz.Action {
	switch t.kind {
	case KindConsole:
		return authz.ConsoleRead
	case KindStats:
		return authz.StatsRead
	case KindState:
		return authz.InstanceView
	case KindJob:
		return authz.Action{}
	default:
		return authz.Action{}
	}
}

// Parse resolves a wire topic to its typed form. It is deliberately exact: there is no
// wildcard, no prefix match and no trailing tolerance, so a client asking for
// `instance.*.state` is told the parameter is invalid rather than handed every instance
// on the host (ADR-040).
func Parse(s string) (Topic, bool) {
	parts := strings.Split(s, ".")
	switch {
	case len(parts) == 2 && parts[0] == "job" && validID(parts[1]):
		return Topic{kind: KindJob, id: parts[1]}, true
	case len(parts) == 3 && parts[0] == "instance" && validID(parts[1]):
		switch parts[2] {
		case "console":
			return Topic{kind: KindConsole, id: parts[1]}, true
		case "stats":
			return Topic{kind: KindStats, id: parts[1]}, true
		case "state":
			return Topic{kind: KindState, id: parts[1]}, true
		}
	}
	return Topic{}, false
}

// validID accepts an id shape and nothing else. The point is not to validate the id — an
// unknown one is not_found either way, decided against the database — but to keep a
// wildcard, a path fragment or a quoted string from ever reaching a resolver, and to keep
// Parse's meaning independent of how ids happen to be minted today (06 §4: UUIDv7).
func validID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
