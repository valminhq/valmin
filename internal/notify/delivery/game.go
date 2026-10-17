package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/notify"
)

// gameEvent is how one ValminEye event is announced: the rule kind that routes it, the
// notification kind it is sent as, and the message a rule without a template gets.
type gameEvent struct {
	rule     alerts.Kind
	kind     notify.Kind
	template string
}

// gameEvents maps each ValminEye event name the panel announces to its announcement. An event
// missing here, such as world_start, is not announced.
var gameEvents = map[string]gameEvent{
	"join":  {alerts.KindPlayerJoined, notify.KindPlayerJoined, "{player} joined"},
	"leave": {alerts.KindPlayerLeft, notify.KindPlayerLeft, "{player} left"},
	"boss_first_kill": {
		alerts.KindBossDefeated, notify.KindBossDefeated,
		"{boss} was defeated for the first time on day {day}. Fighters: {players}",
	},
	"boss_fight_start": {
		alerts.KindBossFightStarted, notify.KindBossFightStarted,
		"{players} started a fight with {boss}",
	},
	"boss_fight_end": {
		alerts.KindBossFightEnded, notify.KindBossFightEnded,
		"The fight with {boss} is over ({outcome}) after {duration} s. Fighters: {players}",
	},
	"raid_start": {alerts.KindRaidStarted, notify.KindRaidStarted, "{name}. Players in the raid: {players}"},
	"raid_end":   {alerts.KindRaidEnded, notify.KindRaidEnded, "The raid is over: {name}"},
}

// playerCooldown is how long after announcing a player's join or leave another one for the
// same player on the same server is dropped.
const playerCooldown = time.Minute

// NotifyGameEvent announces a ValminEye event from instanceID's log to the rules for its kind.
// It does not block: the log reader calls it on its read loop.
func (n *Notifier) NotifyGameEvent(instanceID string, ev instance.EyeEvent) {
	game, ok := gameEvents[ev.Name]
	if !ok {
		return
	}
	if game.rule == alerts.KindPlayerJoined || game.rule == alerts.KindPlayerLeft {
		player, _ := ev.Fields["id"].(string)
		if player == "" {
			player, _ = ev.Fields["player"].(string)
		}
		if !n.players.allow(instanceID+"\x00"+player, cmp.Or(ev.TS, time.Now())) {
			return
		}
	}
	go n.announceGame(context.Background(), instanceID, game, ev)
}

// announceGame sends ev, rendered through each covering rule's template, to the rules' destinations.
func (n *Notifier) announceGame(ctx context.Context, instanceID string, game gameEvent, ev instance.EyeEvent) {
	inst, err := n.DB.InstanceByID(ctx, instanceID)
	if err != nil || inst == nil {
		slog.WarnContext(ctx, "read instance for a game event notification",
			slog.String("instance_id", instanceID), slog.String("event", ev.Name), slog.Any("error", err))
		return
	}
	vars := gameVars(inst.Name, ev.Fields)
	detail := make([]notify.Field, 0, len(vars))
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		detail = append(detail, notify.Field{Name: name, Value: vars[name]})
	}
	n.emitByRules(ctx, game.rule, instanceID, func(p *alerts.ParamsWire) *notify.Event {
		return &notify.Event{
			Kind: game.kind, InstanceID: inst.ID, InstanceName: inst.Name,
			Text:   expand(cmp.Or(p.Template, game.template), vars),
			Detail: detail,
		}
	})
}

// gameVars is the placeholders a template can name: each field of the event as text, a player
// list as names with their deaths, and server as the panel's name for the server. boss and name
// fall back to the game's raw key and raid id, which the plugin sends when the game has no text.
func gameVars(server string, fields map[string]any) map[string]string {
	vars := map[string]string{}
	for name, value := range fields {
		switch value := value.(type) {
		case string:
			vars[name] = value
		case json.Number:
			vars[name] = value.String()
		case bool:
			vars[name] = strconv.FormatBool(value)
		case []any:
			vars[name] = playerList(value)
		}
	}
	delete(vars, "v")
	delete(vars, "event")
	vars["server"] = server
	vars["boss"] = cmp.Or(vars["boss"], vars["key"])
	vars["name"] = cmp.Or(vars["name"], vars["raid"])
	return vars
}

// playerList renders player entries as "Alex (2 deaths), Bo". An entry without a name is left out.
func playerList(entries []any) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		p, _ := entry.(map[string]any)
		name, _ := p["player"].(string)
		if name == "" {
			continue
		}
		deaths, _ := p["deaths"].(json.Number)
		switch d, _ := deaths.Int64(); {
		case d == 1:
			name += " (1 death)"
		case d > 1:
			name = fmt.Sprintf("%s (%d deaths)", name, d)
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// expand replaces each {name} in tmpl with its value in vars. A name vars lacks becomes empty,
// since the plugin's config can turn a field off.
func expand(tmpl string, vars map[string]string) string {
	return placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		return vars[m[1:len(m)-1]]
	})
}

// recentPlayers remembers when each player's join or leave was last announced.
type recentPlayers struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// maxRecentPlayers is the size past which expired entries are swept.
const maxRecentPlayers = 256

// allow reports whether a join or leave for key at may be announced, and records it if so.
//
// ponytail: a reconnect inside the cooldown shows "left" without the "joined" after it; delay
// the leave by the cooldown and drop the pair if that gets reported.
func (r *recentPlayers) allow(key string, at time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.last[key]; ok && at.Sub(last) < playerCooldown {
		return false
	}
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	if len(r.last) >= maxRecentPlayers {
		maps.DeleteFunc(r.last, func(_ string, last time.Time) bool { return at.Sub(last) >= playerCooldown })
	}
	r.last[key] = at
	return true
}
