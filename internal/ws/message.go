package ws

import "time"

// Message is one server→client frame on its way to a subscriber. Seq is the topic's
// sequence number (14 §4.2), carried alongside rather than dug out of the payload so the
// hub can report a gap without knowing what it dropped.
//
// Payload is opaque to the hub (ADR-042). Sources build these; the hub moves them.
type Message struct {
	Seq     uint64
	Payload any
}

// The server→client message types of 04 §4. They are the wire, so the json tags are the
// specification and not a naming preference.

// ConsoleMsg is one log line. Stream is "stdout" or "stderr" — both are forwarded, tagged,
// never merged or deduplicated (14 §4.1).
//
// Line arrives already capped at 14 §3.3's 8 KiB with a truncation marker: the log
// reader does it at reassembly, where the byte budget for the ring buffer is also spent.
// Capping it a second time here would only ever truncate a marker.
type ConsoleMsg struct {
	Type     string    `json:"type"`
	Instance string    `json:"instance"`
	Seq      uint64    `json:"seq"`
	TS       time.Time `json:"ts"`
	Stream   string    `json:"stream"`
	Line     string    `json:"line"`
}

// StatsMsg is one resource sample. The pointer fields are null when the panel does not
// know, which is a different statement from zero (14 §4.3, E10, E7).
type StatsMsg struct {
	Type     string    `json:"type"`
	Instance string    `json:"instance"`
	TS       time.Time `json:"ts"`
	CPUPct   *float64  `json:"cpu_pct"`
	MemBytes uint64    `json:"mem_bytes"`
	MemLimit uint64    `json:"mem_limit"`
	MemPct   *float64  `json:"mem_pct"`
	Players  *int      `json:"players"`
}

// StateMsg is an instance transition (12 §2.2), published after the transaction that wrote
// it commits — announcing one from inside the transaction announces a transition that can
// still roll back (14 §4.4).
type StateMsg struct {
	Type            string `json:"type"`
	Instance        string `json:"instance"`
	State           string `json:"state"`
	RestartRequired bool   `json:"restart_required"`
}

// JobMsg is job progress or a terminal status (12 §7).
type JobMsg struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Message  string `json:"message"`
	// Log is one line of the job's live log tail, empty on a pure progress event.
	Log string `json:"log,omitempty"`
}

// subscribedMsg acknowledges one topic and hands over its current sequence number, which
// is what lets a client reconcile replay against live messages without comparing content.
type subscribedMsg struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
	Seq   uint64 `json:"seq"`
}

// gapMsg is the visible break a lossy topic renders instead of a seamless lie (14 §5).
type gapMsg struct {
	Type    string `json:"type"`
	Topic   string `json:"topic"`
	Dropped int    `json:"dropped"`
	FromSeq uint64 `json:"from_seq"`
}

// resetMsg tells the client the log reader restarted: clear the view, do not splice. The
// sequence numbers deliberately do not restart with it (14 §4.2).
type resetMsg struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
}

// errorMsg is a per-topic failure. It carries a code from 11 §2.5's closed registry, and
// for a resource the caller cannot see that code is not_found rather than forbidden — the
// enumeration oracle is worth closing on the transport that is easier to script against
// (D2, 14 §2.3).
type errorMsg struct {
	Type    string `json:"type"`
	Topic   string `json:"topic,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pongMsg struct {
	Type string `json:"type"`
}

// clientMsg is the whole client→server surface (04 §4). There is deliberately nothing here
// that performs work: a new type that does is a REST endpoint wearing a disguise, and it
// would get neither audit logging nor the error envelope for free (14 §9).
type clientMsg struct {
	Type     string   `json:"type"`
	Topics   []string `json:"topics"`
	Instance string   `json:"instance"`
	Command  string   `json:"command"`
	CSRF     string   `json:"csrf"`
}
