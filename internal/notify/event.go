// Package notify renders panel events into webhook payloads and delivers them to the
// operator's destinations. It imports neither store nor api: an event is a value handed in,
// so the renderers stay testable without a database and a future template layer is a
// renderer over a value that already exists rather than a rewrite of the sender.
//
// Specification: 05 M6, 10 §3, 11 §9.
package notify

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Kind is an event kind — a typed constant with an unexported field, the same closed
// registry authz.Action and jobs.Kind use. A receiver reads the wire name, so it is part of
// the published contract and never renamed.
type Kind struct{ name string }

// String is the wire form, carried in the generic payload as "kind".
func (k Kind) String() string { return k.name }

// MarshalJSON renders the kind as its wire name.
func (k Kind) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(k.name)
	if err != nil {
		return nil, fmt.Errorf("marshal event kind: %w", err)
	}
	return raw, nil
}

var (
	// KindTest is the test send. It is a real delivery down the real path, so an operator
	// who sees it knows the destination and the address policy both work.
	KindTest            = Kind{"test"}
	KindInstanceDown    = Kind{"instance_down"}
	KindUpdateAvailable = Kind{"update_available"}
	KindBackupFailed    = Kind{"backup_failed"}
)

// ParseKind resolves a stored event kind back to the typed constant.
func ParseKind(name string) (Kind, bool) {
	for _, k := range []Kind{KindTest, KindInstanceDown, KindUpdateAvailable, KindBackupFailed} {
		if k.name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// headline is what each kind says. A receiver gets a sentence, not a code.
var headline = map[Kind]string{
	KindTest:            "Test notification",
	KindInstanceDown:    "Server stopped unexpectedly",
	KindUpdateAvailable: "Server update available",
	KindBackupFailed:    "Backup failed",
}

// accent is the provider colour a Discord embed carries, by severity rather than by kind.
var accent = map[Kind]int{
	KindTest:            0x95A5A6,
	KindInstanceDown:    0xD83C3E,
	KindUpdateAvailable: 0x5865F2,
	KindBackupFailed:    0xD83C3E,
}

// Detail bounds. A receiver's body limit is not the panel's to discover at delivery time,
// and a detail field is a label and a short value, never a log excerpt.
const (
	MaxDetailFields = 8
	MaxDetailValue  = 200
)

// Event is one notification, as a structured value. The renderers marshal this; nothing
// assembles JSON as text, so an instance name containing a quote cannot reshape a body
// (05 M6).
type Event struct {
	// ID is stable across retries and across providers, so a receiver that sees a
	// notification twice can tell it is the same one (at-least-once delivery).
	ID           string
	Kind         Kind
	OccurredAt   time.Time
	InstanceID   string
	InstanceName string
	// Detail is the kind's own fields. Bounded on render rather than on construction, so a
	// caller cannot make a body the panel will not send.
	Detail map[string]string
}

// Headline is the event's one-line summary.
func (e *Event) Headline() string {
	h, ok := headline[e.Kind]
	if !ok {
		return "Panel notification"
	}
	return h
}

// bounded returns Detail with at most MaxDetailFields entries, each value truncated to
// MaxDetailValue, ordered so two renders of the same event agree.
func (e *Event) bounded() []field {
	out := make([]field, 0, len(e.Detail))
	for _, k := range slices.Sorted(maps.Keys(e.Detail)) {
		v := e.Detail[k]
		if len(v) > MaxDetailValue {
			v = v[:MaxDetailValue] + "…"
		}
		out = append(out, field{Name: k, Value: v})
		if len(out) == MaxDetailFields {
			break
		}
	}
	return out
}

type field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContentType is what every rendered payload is sent as.
const ContentType = "application/json"

// Body is a rendered payload, ready to POST.
type Body struct {
	ContentType string
	Bytes       []byte
}

// Render builds the payload one destination kind expects.
func Render(destination string, e *Event) (Body, error) {
	var payload any
	switch destination {
	case KindDiscord:
		payload = discordPayload(e)
	case KindGeneric:
		payload = genericPayload(e)
	default:
		return Body{}, fmt.Errorf("unknown destination kind %q", destination)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Body{}, fmt.Errorf("marshal %s payload: %w", destination, err)
	}
	return Body{ContentType: ContentType, Bytes: raw}, nil
}

// Destination kinds, as stored in webhooks.kind.
const (
	KindDiscord = "discord"
	KindGeneric = "generic"
)

// DestinationKinds is the closed set a destination may declare.
var DestinationKinds = []string{KindDiscord, KindGeneric}

// GenericVersion is the schema version generic receivers switch on. It changes only when a
// field's meaning changes; adding a field does not (11 §1, additive changes only).
const GenericVersion = 1

type genericEnvelope struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	Kind       Kind              `json:"kind"`
	OccurredAt string            `json:"occurred_at"`
	Headline   string            `json:"headline"`
	Instance   *genericInstance  `json:"instance"`
	Detail     map[string]string `json:"detail"`
}

type genericInstance struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func genericPayload(e *Event) genericEnvelope {
	env := genericEnvelope{
		Version:    GenericVersion,
		ID:         e.ID,
		Kind:       e.Kind,
		OccurredAt: e.OccurredAt.UTC().Format(time.RFC3339),
		Headline:   e.Headline(),
		Detail:     map[string]string{},
	}
	if e.InstanceID != "" {
		env.Instance = &genericInstance{ID: e.InstanceID, Name: e.InstanceName}
	}
	for _, f := range e.bounded() {
		env.Detail[f.Name] = f.Value
	}
	return env
}

type discordMessage struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title     string  `json:"title"`
	Color     int     `json:"color"`
	Timestamp string  `json:"timestamp"`
	Fields    []field `json:"fields"`
}

func discordPayload(e *Event) discordMessage {
	fields := make([]field, 0, MaxDetailFields+1)
	if e.InstanceName != "" {
		fields = append(fields, field{Name: "Server", Value: e.InstanceName})
	}
	fields = append(fields, e.bounded()...)
	return discordMessage{Embeds: []discordEmbed{{
		Title:     e.Headline(),
		Color:     accent[e.Kind],
		Timestamp: e.OccurredAt.UTC().Format(time.RFC3339),
		Fields:    fields,
	}}}
}
