// Package notify renders panel events into webhook payloads and delivers them to the
// operator's destinations. It imports neither store nor api: an event is a value handed in,
// so the renderers stay testable without a database and a future template layer is a
// renderer over a value that already exists rather than a rewrite of the sender.
//
// Specification: 05 M6, 10 §3, 11 §9.
package notify

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// KindPowerCutSoon is a planned power cut close enough that every running server is about
	// to stop.
	KindPowerCutSoon = Kind{"power_cut_soon"}
	// KindServerStarted and KindServerStopped report a start or stop the panel made.
	KindServerStarted = Kind{"server_started"}
	KindServerStopped = Kind{"server_stopped"}
	// KindAlertOpened and KindAlertResolved are the two edges of an operational condition. One
	// kind per edge rather than per condition: the condition names itself in Summary and
	// Detail, and a receiver filtering on kind wants "something broke" and "it cleared".
	KindAlertOpened   = Kind{"alert_opened"}
	KindAlertResolved = Kind{"alert_resolved"}
	// Game events ValminEye reports from inside the game, under the alert rule kinds' names.
	KindPlayerJoined     = Kind{"player_joined"}
	KindPlayerLeft       = Kind{"player_left"}
	KindBossDefeated     = Kind{"boss_defeated"}
	KindBossFightStarted = Kind{"boss_fight_started"}
	KindBossFightEnded   = Kind{"boss_fight_ended"}
	KindRaidStarted      = Kind{"raid_started"}
	KindRaidEnded        = Kind{"raid_ended"}
)

// ParseKind resolves a stored event kind back to the typed constant.
func ParseKind(name string) (Kind, bool) {
	for _, k := range []Kind{
		KindTest, KindInstanceDown, KindUpdateAvailable, KindBackupFailed,
		KindAlertOpened, KindAlertResolved, KindPowerCutSoon,
		KindServerStarted, KindServerStopped,
		KindPlayerJoined, KindPlayerLeft, KindBossDefeated, KindBossFightStarted, KindBossFightEnded,
		KindRaidStarted, KindRaidEnded,
	} {
		if k.name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// headline is what each kind says. A receiver gets a sentence, not a code.
var headline = map[Kind]string{
	KindTest:             "Test notification",
	KindInstanceDown:     "Server stopped unexpectedly",
	KindUpdateAvailable:  "Server update available",
	KindBackupFailed:     "Backup failed",
	KindAlertOpened:      "Something needs attention",
	KindAlertResolved:    "Resolved",
	KindPowerCutSoon:     "Power cut soon: servers are stopping",
	KindServerStarted:    "Server started",
	KindServerStopped:    "Server stopped",
	KindPlayerJoined:     "Player joined",
	KindPlayerLeft:       "Player left",
	KindBossDefeated:     "Boss defeated",
	KindBossFightStarted: "Boss fight started",
	KindBossFightEnded:   "Boss fight ended",
	KindRaidStarted:      "Raid started",
	KindRaidEnded:        "Raid ended",
}

// accent is the provider colour a Discord embed carries, by severity rather than by kind.
var accent = map[Kind]int{
	KindTest:             0x95A5A6,
	KindInstanceDown:     0xD83C3E,
	KindUpdateAvailable:  0x5865F2,
	KindBackupFailed:     0xD83C3E,
	KindAlertOpened:      0xD83C3E,
	KindAlertResolved:    0x2ECC71,
	KindPowerCutSoon:     0xF1C40F,
	KindServerStarted:    0x2ECC71,
	KindServerStopped:    0x95A5A6,
	KindPlayerJoined:     0x2ECC71,
	KindPlayerLeft:       0x95A5A6,
	KindBossDefeated:     0xF1C40F,
	KindBossFightStarted: 0xE67E22,
	KindBossFightEnded:   0x5865F2,
	KindRaidStarted:      0xD83C3E,
	KindRaidEnded:        0x2ECC71,
}

// Detail bounds. A receiver's body limit is not the panel's to discover at delivery time,
// and a detail field is a label and a short value, never a log excerpt.
const (
	MaxDetailFields = 8
	MaxDetailValue  = 200
	// MaxText bounds an event's Text, well inside Discord's 4096-character embed description.
	MaxText = 2000
	// maxContent is Discord's limit on a plain message, in characters.
	maxContent = 2000
)

// Message layouts a rule can choose for Discord.
const (
	// LayoutLine is a card titled with the server and the headline on one line.
	LayoutLine = ""
	// LayoutMessage is a card holding only the message.
	LayoutMessage = "message"
	// LayoutPlain is the message as plain text, without a card.
	LayoutPlain = "plain"
)

// Layouts is the closed set of layouts a rule may choose.
var Layouts = []string{LayoutLine, LayoutMessage, LayoutPlain}

// Style is how an event's Discord message looks. The zero value is a card titled with the server
// and the headline, linked to the panel, with the time.
type Style struct {
	Layout   string
	HideLink bool
	HideTime bool
	// Color is the card's colour as #RRGGBB, or empty for the kind's own.
	Color string
}

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
	// Text is the message body below the headline in Discord markdown, or empty for a kind that
	// has none.
	Text string
	// PlainText is Text without markdown or escapes, for receivers that are not Discord.
	PlainText string
	// URL is the panel page the event is about, or empty when the panel has no address to give.
	URL string
	// Detail is the kind's own fields, in the order a reader should see them. Bounded on render
	// rather than on construction, so a caller cannot make a body the panel will not send.
	Detail []Field
	// Style is how the Discord message looks.
	Style Style
	// Resolves is the id of the alert_opened event an alert_resolved event clears, or empty.
	Resolves string
}

// Field is one labelled value of an event's detail. Key names it in a message template, and is
// empty for a field no template can show.
type Field struct {
	Key   string `json:"-"`
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

// truncate cuts s to n characters, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// bounded returns the first MaxDetailFields entries of Detail that have a value, each truncated
// to MaxDetailValue characters.
func (e *Event) bounded() []Field {
	out := make([]Field, 0, min(len(e.Detail), MaxDetailFields))
	for _, f := range e.Detail {
		if f.Value == "" {
			continue
		}
		f.Value = truncate(f.Value, MaxDetailValue)
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
	Text       string            `json:"text,omitempty"`
	URL        string            `json:"url,omitempty"`
	Resolves   string            `json:"resolves,omitempty"`
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
		Text:       truncate(e.PlainText, MaxText),
		URL:        e.URL,
		Resolves:   e.Resolves,
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

// AllowedMentions is Discord's allowed_mentions object, which decides which mentions in a
// message notify anyone.
type AllowedMentions struct {
	Parse []string `json:"parse"`
}

// NoMentions notifies nobody. Every message the panel posts to Discord carries it, so text that
// names @everyone, @here or a role, from a template or a server or player name, pings no one.
var NoMentions = AllowedMentions{Parse: []string{}}

type discordMessage struct {
	// Content is the plain text above any card, which Discord renders as markdown.
	Content string `json:"content,omitempty"`
	// Embeds is always sent, so editing a card into plain text removes the card.
	Embeds          []discordEmbed  `json:"embeds"`
	AllowedMentions AllowedMentions `json:"allowed_mentions"`
}

type discordEmbed struct {
	Title string `json:"title,omitempty"`
	// Description is the embed body, which Discord renders as markdown.
	Description string  `json:"description,omitempty"`
	URL         string  `json:"url,omitempty"`
	Color       int     `json:"color"`
	Timestamp   string  `json:"timestamp,omitempty"`
	Fields      []Field `json:"fields"`
}

func discordPayload(e *Event) discordMessage {
	text := e.Text
	if e.Kind == KindAlertResolved && e.Style.Layout != LayoutLine {
		// Without a title, the message itself has to say the alert cleared.
		text = strings.TrimSuffix("**"+e.Headline()+"**\n"+text, "\n")
	}
	if e.Style.Layout == LayoutPlain {
		content := truncate(cmp.Or(text, e.Headline()), maxContent-1)
		return discordMessage{Content: content, Embeds: []discordEmbed{}, AllowedMentions: NoMentions}
	}
	text = truncate(text, MaxText)
	embed := discordEmbed{Description: text, Color: e.color(), Fields: e.bounded()}
	// The text already says what the fields hold.
	if text != "" {
		embed.Fields = []Field{}
	}
	if !e.Style.HideTime {
		embed.Timestamp = e.OccurredAt.UTC().Format(time.RFC3339)
	}
	if e.Style.Layout == LayoutMessage {
		embed.Description = cmp.Or(text, e.Headline())
		return discordMessage{Embeds: []discordEmbed{embed}, AllowedMentions: NoMentions}
	}
	embed.Title = e.Headline()
	if e.InstanceName != "" {
		embed.Title = e.InstanceName + " · " + embed.Title
	}
	if !e.Style.HideLink {
		embed.URL = e.URL
	}
	return discordMessage{Embeds: []discordEmbed{embed}, AllowedMentions: NoMentions}
}

// color is the card's colour: the style's, or else the kind's accent.
func (e *Event) color() int {
	if c, err := strconv.ParseUint(strings.TrimPrefix(e.Style.Color, "#"), 16, 24); err == nil {
		return int(c)
	}
	return accent[e.Kind]
}
