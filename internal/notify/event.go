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
	"time"
	"unicode/utf8"
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
	// KindInstanceAutoStopped is a stop the panel made because the server had no players.
	KindInstanceAutoStopped = Kind{"instance_auto_stopped"}
	// KindPowerCutSoon is a planned power cut close enough that every running server is about
	// to stop.
	KindPowerCutSoon = Kind{"power_cut_soon"}
	// KindAlertOpened and KindAlertResolved are the two edges of an operational condition. One
	// kind per edge rather than per condition: the condition names itself in Summary and
	// Detail, and a receiver filtering on kind wants "something broke" and "it cleared".
	KindAlertOpened   = Kind{"alert_opened"}
	KindAlertResolved = Kind{"alert_resolved"}
)

// ParseKind resolves a stored event kind back to the typed constant.
func ParseKind(name string) (Kind, bool) {
	for _, k := range []Kind{
		KindTest, KindInstanceDown, KindUpdateAvailable, KindBackupFailed,
		KindAlertOpened, KindAlertResolved, KindInstanceAutoStopped, KindPowerCutSoon,
	} {
		if k.name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// headline is what each kind says. A receiver gets a sentence, not a code.
var headline = map[Kind]string{
	KindTest:                "Test notification",
	KindInstanceDown:        "Server stopped unexpectedly",
	KindUpdateAvailable:     "Server update available",
	KindBackupFailed:        "Backup failed",
	KindAlertOpened:         "Something needs attention",
	KindAlertResolved:       "Resolved",
	KindInstanceAutoStopped: "Server stopped: no players",
	KindPowerCutSoon:        "Power cut soon: servers are stopping",
}

// accent is the provider colour a Discord embed carries, by severity rather than by kind.
var accent = map[Kind]int{
	KindTest:                0x95A5A6,
	KindInstanceDown:        0xD83C3E,
	KindUpdateAvailable:     0x5865F2,
	KindBackupFailed:        0xD83C3E,
	KindAlertOpened:         0xD83C3E,
	KindAlertResolved:       0x2ECC71,
	KindInstanceAutoStopped: 0x95A5A6,
	KindPowerCutSoon:        0xF1C40F,
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
	// Summary overrides the kind's stock headline, for a kind that covers many situations.
	Summary string
	// URL is the panel page the event is about, or empty when the panel has no address to give.
	URL string
	// Detail is the kind's own fields, in the order a reader should see them. Bounded on render
	// rather than on construction, so a caller cannot make a body the panel will not send.
	Detail []Field
}

// Field is one labelled value of an event's detail.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Headline is the event's one-line summary.
func (e *Event) Headline() string {
	if e.Summary != "" {
		return e.Summary
	}
	h, ok := headline[e.Kind]
	if !ok {
		return "Panel notification"
	}
	return h
}

// bounded returns the first MaxDetailFields entries of Detail that have a value, each truncated
// to MaxDetailValue characters.
func (e *Event) bounded() []Field {
	out := make([]Field, 0, min(len(e.Detail), MaxDetailFields))
	for _, f := range e.Detail {
		if f.Value == "" {
			continue
		}
		if utf8.RuneCountInString(f.Value) > MaxDetailValue {
			f.Value = string([]rune(f.Value)[:MaxDetailValue]) + "…"
		}
		out = append(out, f)
		if len(out) == MaxDetailFields {
			break
		}
	}
	return out
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
	URL        string            `json:"url,omitempty"`
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
		URL:        e.URL,
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
	Author    *discordAuthor `json:"author,omitempty"`
	Title     string         `json:"title"`
	URL       string         `json:"url,omitempty"`
	Color     int            `json:"color"`
	Timestamp string         `json:"timestamp"`
	Fields    []Field        `json:"fields"`
}

// discordAuthor is the line Discord shows above the title, which is where the server is named.
type discordAuthor struct {
	Name string `json:"name"`
}

func discordPayload(e *Event) discordMessage {
	embed := discordEmbed{
		Title:     e.Headline(),
		URL:       e.URL,
		Color:     accent[e.Kind],
		Timestamp: e.OccurredAt.UTC().Format(time.RFC3339),
		Fields:    e.bounded(),
	}
	if e.InstanceName != "" {
		embed.Author = &discordAuthor{Name: e.InstanceName}
	}
	return discordMessage{Embeds: []discordEmbed{embed}}
}
