package delivery

import (
	"cmp"
	"regexp"
	"strings"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/notify"
)

// defaultTemplates is the message a rule without a template sends, by rule kind. Every kind a
// rule can route has one.
var defaultTemplates = map[alerts.Kind]string{
	alerts.KindJobFailed:       "**Reason:** {reason}\n**What to do:** {what_to_do}",
	alerts.KindLowDisk:         "**Free space:** {free}\n**Alert below:** {alarm}\n**What to do:** {what_to_do}",
	alerts.KindStaleBackup:     "**Schedule:** {schedule}\n**Last backup:** {last_backup}\n**What to do:** {what_to_do}",
	alerts.KindUncleanStop:     "**Job:** {job}\n**What to do:** {what_to_do}",
	alerts.KindRestartRequired: "**What to do:** {what_to_do}",
	alerts.KindUpdateAvailable: "**Installed build:** {installed}\n**Available build:** {available}\n" +
		"**What to do:** {what_to_do}",
	alerts.KindInstanceError: "**What to do:** {what_to_do}",
	alerts.KindCrashLoop:     "**Unexpected stops:** {stops}\n**What to do:** {what_to_do}",
	alerts.KindJobStuck:      "**Running for:** {running}\n**What to do:** {what_to_do}",
	alerts.KindPowerCut:      "**Power goes off:** {power_off}\n**Servers stop:** {stop_at}\n**Running servers:** {servers}",
	alerts.KindServerStarted: "**Server name:** {server_name}\n**World:** {world}\n**Join code:** {join_code}\n" +
		"**Port:** {port}\n**Started:** {time}",
	alerts.KindServerStopped: "**Reason:** {reason}\n**Server name:** {server_name}\n**World:** {world}\n" +
		"**Stopped:** {time}",
	alerts.KindPlayerJoined:     "{player} joined",
	alerts.KindPlayerLeft:       "{player} left",
	alerts.KindBossDefeated:     "{boss} was defeated for the first time on day {day}. Fighters: {players}",
	alerts.KindBossFightStarted: "{players} started a fight with {boss}",
	alerts.KindBossFightEnded:   "The fight with {boss} is over ({outcome}) after {duration} s. Fighters: {players}",
	alerts.KindRaidStarted:      "{name}. Players in the raid: {players}",
	alerts.KindRaidEnded:        "The raid is over: {name}",
}

// compose writes a rule's message into an event: the rule's template, or else the kind's default,
// over the event's keyed fields, and the rule's message style. Besides the fields, a template can
// name {server} and {event}, the headline.
func compose(e *notify.Event, kind alerts.Kind, p *alerts.ParamsWire) {
	vars := map[string]string{"server": e.InstanceName, "event": e.Headline()}
	for _, f := range e.Detail {
		if f.Key != "" {
			vars[f.Key] = f.Value
		}
	}
	for name, value := range vars {
		vars[name] = escapeMarkdown(value)
	}
	e.Text = expand(cmp.Or(p.Template, defaultTemplates[kind]), vars)
	e.Style = notify.Style{Layout: p.Layout, HideLink: p.HideLink, HideTime: p.HideTime, Color: p.Color}
}

// markdown is what Discord would read as formatting, a link or a mention in a field value.
var markdown = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "<", `\<`,
	"[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "#", `\#`, "-", `\-`, "\n", " ", "\r", " ",
)

// escapeMarkdown makes a field value, which a player or a recorded error chose, read as plain
// text in the message.
func escapeMarkdown(s string) string { return markdown.Replace(s) }

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// expand replaces each {name} in tmpl with its value in vars. A name vars lacks becomes empty,
// since a rule can hide a field and the plugin's config can turn one off, and a line whose every
// placeholder came out empty is left out.
func expand(tmpl string, vars map[string]string) string {
	lines := strings.Split(tmpl, "\n")
	kept := lines[:0]
	for _, line := range lines {
		named, filled := false, false
		line = placeholder.ReplaceAllStringFunc(line, func(m string) string {
			value := vars[m[1:len(m)-1]]
			named = true
			filled = filled || value != ""
			return value
		})
		if named && !filled {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
