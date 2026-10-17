package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/notify"
)

// gameEvent is how one ValminEye event is announced: the rule kind that routes it and the
// notification kind it is sent as.
type gameEvent struct {
	rule alerts.Kind
	kind notify.Kind
}

// gameEvents maps each ValminEye event name the panel announces to its announcement. An event
// missing here, such as world_start, is not announced.
var gameEvents = map[string]gameEvent{
	"join":             {alerts.KindPlayerJoined, notify.KindPlayerJoined},
	"leave":            {alerts.KindPlayerLeft, notify.KindPlayerLeft},
	"boss_first_kill":  {alerts.KindBossDefeated, notify.KindBossDefeated},
	"boss_fight_start": {alerts.KindBossFightStarted, notify.KindBossFightStarted},
	"boss_fight_end":   {alerts.KindBossFightEnded, notify.KindBossFightEnded},
	"raid_start":       {alerts.KindRaidStarted, notify.KindRaidStarted},
	"raid_end":         {alerts.KindRaidEnded, notify.KindRaidEnded},
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

// announceGame sends ev to the destinations of the rules covering it.
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
		detail = append(detail, notify.Field{Key: name, Name: name, Value: vars[name]})
	}
	n.emitByRules(ctx, game.rule, instanceID, func(*alerts.ParamsWire) *notify.Event {
		return &notify.Event{Kind: game.kind, InstanceID: inst.ID, InstanceName: inst.Name, Detail: detail}
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
