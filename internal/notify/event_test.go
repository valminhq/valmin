package notify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testEvent() *Event {
	return &Event{
		ID:           "01920000-0000-7000-8000-000000000001",
		Kind:         KindInstanceDown,
		OccurredAt:   time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC),
		InstanceID:   "01920000-0000-7000-8000-000000000002",
		InstanceName: `Bob's "Valheim" server`,
		URL:          "https://panel.example/instances/01920000-0000-7000-8000-000000000002",
		Detail:       []Field{{Name: "Exit code", Value: "137"}},
	}
}

// TestARenderedPayloadCannotBeReshapedByItsValues asserts 05 M6's structural rule: the body is
// built and marshalled, never assembled as text, so a server name carrying a quote is a string
// value rather than a way to add fields.
func TestARenderedPayloadCannotBeReshapedByItsValues(t *testing.T) {
	for _, kind := range DestinationKinds {
		body, err := Render(kind, testEvent())
		if err != nil {
			t.Fatalf("Render(%s): %v", kind, err)
		}
		var decoded any
		if err := json.Unmarshal(body.Bytes, &decoded); err != nil {
			t.Fatalf("%s payload does not decode: %v", kind, err)
		}
		if !strings.Contains(string(body.Bytes), `Bob's \"Valheim\" server`) {
			t.Errorf("%s payload = %s, want the name escaped rather than interpolated", kind, body.Bytes)
		}
	}
}

// TestTheGenericPayloadCarriesTheStableEventIdentity asserts what a generic receiver switches
// on: a schema version, the event id that is stable across retries, the kind and the instance.
func TestTheGenericPayloadCarriesTheStableEventIdentity(t *testing.T) {
	body, err := Render(KindGeneric, testEvent())
	if err != nil {
		t.Fatal(err)
	}
	// Decoded into a shadow of the envelope: nothing in the panel reads a payload back, so
	// Kind is a wire name on the way out only.
	var got struct {
		Version    int               `json:"version"`
		ID         string            `json:"id"`
		Kind       string            `json:"kind"`
		OccurredAt string            `json:"occurred_at"`
		URL        string            `json:"url"`
		Instance   *genericInstance  `json:"instance"`
		Detail     map[string]string `json:"detail"`
	}
	if err := json.Unmarshal(body.Bytes, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != GenericVersion {
		t.Errorf("version = %d, want %d", got.Version, GenericVersion)
	}
	if got.ID != testEvent().ID || got.Kind != KindInstanceDown.String() {
		t.Errorf("identity = %s/%s, want the event's own", got.ID, got.Kind)
	}
	if got.OccurredAt != "2026-09-12T02:00:00Z" {
		t.Errorf("occurred_at = %q, want RFC3339 UTC", got.OccurredAt)
	}
	if got.Instance == nil || got.Instance.ID != testEvent().InstanceID {
		t.Errorf("instance = %+v, want the event's", got.Instance)
	}
	if got.URL != testEvent().URL {
		t.Errorf("url = %q, want the event's link", got.URL)
	}
	if got.Detail["Exit code"] != "137" {
		t.Errorf("detail = %v, want the event's", got.Detail)
	}
}

// TestDetailIsBoundedOnRender asserts that a caller cannot build a body the panel will not
// send: too many fields are dropped and a long value is truncated.
func TestDetailIsBoundedOnRender(t *testing.T) {
	e := testEvent()
	e.Detail = []Field{{Name: "Long", Value: strings.Repeat("x", MaxDetailValue*2)}}
	for i := range MaxDetailFields * 2 {
		e.Detail = append(e.Detail, Field{Name: string(rune('a' + i)), Value: "v"})
	}
	fields := e.bounded()
	if len(fields) != MaxDetailFields {
		t.Errorf("fields = %d, want %d", len(fields), MaxDetailFields)
	}
	e.Detail = []Field{{Name: "Long", Value: strings.Repeat("é", MaxDetailValue*2)}}
	if v := e.bounded()[0].Value; !utf8.ValidString(v) || len([]rune(v)) != MaxDetailValue+1 {
		t.Errorf("value length = %d, want it truncated to %d", len([]rune(v)), MaxDetailValue)
	}
}

// TestTheDiscordEmbedNamesTheServerAndLinksThePanel asserts the embed shape a reader sees by
// default: the server and the headline on one title line linking to the panel, and the detail in
// the order it was written with empty values left out.
func TestTheDiscordEmbedNamesTheServerAndLinksThePanel(t *testing.T) {
	e := testEvent()
	e.Detail = []Field{
		{Name: "Status", Value: "Stopped"},
		{Name: "Cause", Value: ""},
		{Name: "What to do", Value: "Start it."},
	}
	body, err := Render(KindDiscord, e)
	if err != nil {
		t.Fatal(err)
	}
	var got discordMessage
	if err := json.Unmarshal(body.Bytes, &got); err != nil {
		t.Fatal(err)
	}
	embed := got.Embeds[0]
	if want := e.InstanceName + " · Server stopped unexpectedly"; embed.URL != e.URL || embed.Title != want {
		t.Errorf("title = %q linking %q, want %q linking the panel", embed.Title, embed.URL, want)
	}
	want := []Field{{Name: "Status", Value: "Stopped"}, {Name: "What to do", Value: "Start it."}}
	if len(embed.Fields) != len(want) || embed.Fields[0] != want[0] || embed.Fields[1] != want[1] {
		t.Errorf("fields = %+v, want %+v", embed.Fields, want)
	}
}

// TestTheTextIsTheDiscordDescriptionAndTheGenericText asserts both renderers carry an event's
// text, bounded to MaxText characters, and that Discord then shows no fields.
func TestTheTextIsTheDiscordDescriptionAndTheGenericText(t *testing.T) {
	e := testEvent()
	e.Kind = KindPlayerJoined
	e.Text = strings.Repeat("ä", MaxText+50)
	want := strings.Repeat("ä", MaxText) + "…"

	body, err := Render(KindDiscord, e)
	if err != nil {
		t.Fatal(err)
	}
	var discord discordMessage
	if err := json.Unmarshal(body.Bytes, &discord); err != nil {
		t.Fatal(err)
	}
	if got := discord.Embeds[0]; got.Description != want || !strings.HasSuffix(got.Title, " · Player joined") ||
		len(got.Fields) != 0 {
		t.Errorf("embed = %q / %d characters / %d fields, want the headline over the bounded text alone",
			got.Title, utf8.RuneCountInString(got.Description), len(got.Fields))
	}

	body, err = Render(KindGeneric, e)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body.Bytes, &generic); err != nil {
		t.Fatal(err)
	}
	if generic.Text != want {
		t.Errorf("generic text has %d characters, want the bounded text", utf8.RuneCountInString(generic.Text))
	}
}

// TestAStyleShapesTheDiscordMessage asserts what each layout and option leaves in the message.
func TestAStyleShapesTheDiscordMessage(t *testing.T) {
	plain := strings.Repeat("a", maxContent+10)
	tests := []struct {
		name  string
		style Style
		text  string
		want  discordMessage
	}{
		{
			name:  "one line without link or time",
			style: Style{HideLink: true, HideTime: true, Color: "#00ff00"},
			text:  "Alex joined",
			want: discordMessage{Embeds: []discordEmbed{{
				Title: `Bob's "Valheim" server · Player joined`, Description: "Alex joined", Color: 0x00FF00,
				Fields: []Field{},
			}}},
		},
		{
			name:  "message only",
			style: Style{Layout: LayoutMessage},
			text:  "Alex joined",
			want: discordMessage{Embeds: []discordEmbed{{
				Description: "Alex joined", Color: accent[KindPlayerJoined],
				Timestamp: "2026-09-12T02:00:00Z", Fields: []Field{},
			}}},
		},
		{
			name:  "message only falls back to the headline",
			style: Style{Layout: LayoutMessage, HideTime: true},
			want: discordMessage{Embeds: []discordEmbed{{
				Description: "Player joined", Color: accent[KindPlayerJoined], Fields: []Field{},
			}}},
		},
		{
			name:  "plain text",
			style: Style{Layout: LayoutPlain, Color: "#00ff00"},
			text:  "Alex joined",
			want:  discordMessage{Content: "Alex joined", Embeds: []discordEmbed{}},
		},
		{
			name:  "plain text inside Discord's limit",
			style: Style{Layout: LayoutPlain},
			text:  plain,
			want:  discordMessage{Content: plain[:maxContent-1] + "…", Embeds: []discordEmbed{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := testEvent()
			e.Kind, e.Detail, e.Text, e.Style = KindPlayerJoined, nil, tt.text, tt.style
			body, err := Render(KindDiscord, e)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body.Bytes, want) {
				t.Errorf("payload = %s, want %s", body.Bytes, want)
			}
		})
	}
}

// TestAResolvedAlertSaysSoAndPointsAtItsAlert asserts a resolution laid out without a title still
// leads with its headline, and that the generic payload names the alert it resolves.
func TestAResolvedAlertSaysSoAndPointsAtItsAlert(t *testing.T) {
	e := testEvent()
	e.Kind, e.Summary, e.Text, e.Detail = KindAlertResolved, "Resolved: Low disk", "Free: 1 GB", nil
	e.Resolves, e.Style = "01920000-0000-7000-8000-000000000009", Style{Layout: LayoutPlain}

	body, err := Render(KindDiscord, e)
	if err != nil {
		t.Fatal(err)
	}
	var discord discordMessage
	if err := json.Unmarshal(body.Bytes, &discord); err != nil {
		t.Fatal(err)
	}
	if want := "**Resolved: Low disk**\nFree: 1 GB"; discord.Content != want {
		t.Errorf("content = %q, want %q", discord.Content, want)
	}

	body, err = Render(KindGeneric, e)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Resolves string `json:"resolves"`
	}
	if err := json.Unmarshal(body.Bytes, &generic); err != nil {
		t.Fatal(err)
	}
	if generic.Resolves != e.Resolves {
		t.Errorf("resolves = %q, want %q", generic.Resolves, e.Resolves)
	}
}
