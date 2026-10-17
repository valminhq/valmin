package delivery

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/notify"
)

// eyeFields decodes a ValminEye object the way the log reader does.
func eyeFields(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestGameVarsNameEveryFieldAMessageCanShow(t *testing.T) {
	tests := []struct {
		raw  string
		want map[string]string
	}{
		{
			`{"v":1,"event":"boss_first_kill","key":"defeated_eikthyr","boss":"Eikthyr","day":12,` +
				`"players":[{"player":"Alex","id":"Steam_1","deaths":2},{"player":"Bo","id":"Steam_2","deaths":0},` +
				`{"player":"Cy","id":"Steam_3","deaths":1}]}`,
			map[string]string{
				"server": "Vikings", "key": "defeated_eikthyr", "boss": "Eikthyr", "day": "12",
				"players": "Alex (2 deaths), Bo, Cy (1 death)", "name": "",
			},
		},
		{
			`{"v":1,"event":"boss_fight_start","key":"defeated_modboss","first":true}`,
			map[string]string{
				"server": "Vikings", "key": "defeated_modboss", "boss": "defeated_modboss",
				"first": "true", "name": "",
			},
		},
		{
			`{"v":1,"event":"raid_end","raid":"army_eikthyr","players":[]}`,
			map[string]string{
				"server": "Vikings", "raid": "army_eikthyr", "name": "army_eikthyr",
				"players": "", "boss": "",
			},
		},
	}
	for _, tt := range tests {
		if got := gameVars("Vikings", eyeFields(t, tt.raw)); !maps.Equal(got, tt.want) {
			t.Errorf("gameVars(%s) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestExpandFillsPlaceholdersAndKeepsOtherBraces(t *testing.T) {
	vars := map[string]string{"player": "Alex", "day": "12"}
	tests := []struct{ tmpl, want string }{
		{"{player} joined on day {day}", "Alex joined on day 12"},
		{"{player} died {deaths} times", "Alex died  times"},
		{"{Player} {} {player:x} {{player}}", "{Player} {} {player:x} {Alex}"},
		{"**Player:** {player}\n**Deaths:** {deaths}\nGood luck", "**Player:** Alex\nGood luck"},
		{"{deaths}", ""},
		{"Fighters:\n{deaths} {wipes}\n\n{day}", "Fighters:\n\n12"},
	}
	for _, tt := range tests {
		if got := expand(tt.tmpl, vars); got != tt.want {
			t.Errorf("expand(%q) = %q, want %q", tt.tmpl, got, tt.want)
		}
	}
}

func TestEveryRuleKindHasADefaultMessage(t *testing.T) {
	kinds := slices.Concat(alerts.Kinds(), []alerts.Kind{
		alerts.KindPowerCut, alerts.KindServerStarted, alerts.KindServerStopped,
	})
	for _, game := range gameEvents {
		kinds = append(kinds, game.rule)
	}
	for _, k := range kinds {
		if defaultTemplates[k] == "" {
			t.Errorf("%s has no default message", k)
		}
	}
}

func TestEveryAnnouncedEventIsSentUnderItsRuleKind(t *testing.T) {
	for name, game := range gameEvents {
		if game.rule.String() != game.kind.String() {
			t.Errorf("%s routes as %s, sends %s", name, game.rule, game.kind)
		}
	}
	if _, ok := gameEvents["world_start"]; ok {
		t.Error("world_start is announced; the server-started notification covers it")
	}
}

// TestAPlayerIsAnnouncedOncePerCooldown asserts a second join or leave for the same player on the
// same server inside the cooldown is dropped, and other players and servers are not.
func TestAPlayerIsAnnouncedOncePerCooldown(t *testing.T) {
	var r recentPlayers
	at := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	steps := []struct {
		key   string
		after time.Duration
		want  bool
	}{
		{"a\x00Steam_1", 0, true},
		{"a\x00Steam_1", 30 * time.Second, false},
		{"a\x00Steam_2", 30 * time.Second, true},
		{"b\x00Steam_1", 30 * time.Second, true},
		{"a\x00Steam_1", playerCooldown, true},
	}
	for _, s := range steps {
		if got := r.allow(s.key, at.Add(s.after)); got != s.want {
			t.Errorf("allow(%q, +%s) = %v, want %v", s.key, s.after, got, s.want)
		}
	}
}

// TestAPlayerNameCannotFormatTheMessage asserts a field value carrying Discord markdown, a link or
// a line break reads as plain text.
func TestAPlayerNameCannotFormatTheMessage(t *testing.T) {
	tests := []struct{ value, want string }{
		{"[gold](https://evil.example)", `\[gold\]\(https://evil.example\)`},
		{"**Bo**_`x`_", "\\*\\*Bo\\*\\*\\_\\`x\\`\\_"},
		{"<@123> ||a|| ~~b~~", `\<@123\> \|\|a\|\| \~\~b\~\~`},
		{"# big\n> quote", `\# big \> quote`},
		{`a\b`, `a\\b`},
		{"Alex", "Alex"},
	}
	for _, tt := range tests {
		if got := escapeMarkdown(tt.value); got != tt.want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

// TestARuleComposesTheMessageFromItsTemplateAndStyle asserts a rule's message: its template over
// the event's keyed fields with values escaped, {server} and {event} filled in, a hidden field's
// line left out, the kind's default without a template, and the rule's style carried over.
func TestARuleComposesTheMessageFromItsTemplateAndStyle(t *testing.T) {
	event := func() *notify.Event {
		return &notify.Event{
			Kind: notify.KindServerStopped, InstanceName: "Vikings_1",
			Detail: []notify.Field{
				{Key: alerts.FieldReason, Name: "Reason", Value: "Stopped by *bob*"},
				{Key: alerts.FieldTime, Name: "Stopped", Value: "17 Oct 2026, 15:12 UTC"},
			},
		}
	}
	tests := []struct {
		name   string
		params alerts.ParamsWire
		want   string
	}{
		{
			name:   "template",
			params: alerts.ParamsWire{Template: "{server}: {event} ({reason})\nWorld: {world}"},
			want:   `Vikings\_1: Server stopped (Stopped by \*bob\*)`,
		},
		{
			name: "default",
			want: "**Reason:** Stopped by \\*bob\\*\n**Stopped:** 17 Oct 2026, 15:12 UTC",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := event()
			p := tt.params
			p.Layout, p.HideLink, p.Color = notify.LayoutPlain, true, "#123456"
			compose(e, alerts.KindServerStopped, &p)
			if e.Text != tt.want {
				t.Errorf("text = %q, want %q", e.Text, tt.want)
			}
			if want := (notify.Style{Layout: notify.LayoutPlain, HideLink: true, Color: "#123456"}); e.Style != want {
				t.Errorf("style = %+v, want %+v", e.Style, want)
			}
		})
	}
}
