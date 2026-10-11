package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Interaction and response types the bot handles.
const (
	interactionCommand      = 2
	interactionAutocomplete = 4

	responseMessage      = 4
	responseAutocomplete = 8

	flagEphemeral = 64
	maxChoices    = 25
)

// tryLater answers an interaction the bot could not serve.
const tryLater = "Something went wrong. Try again later."

// Channel types that are threads, whose parent channel's link applies to them.
var threadTypes = []int{10, 11, 12}

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

// option is a command option, or a subcommand with options of its own.
type option struct {
	Name    string          `json:"name"`
	Value   json.RawMessage `json:"value"`
	Focused bool            `json:"focused"`
	Options []option        `json:"options"`
}

// interaction is the part of an INTERACTION_CREATE the bot reads.
type interaction struct {
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	Type          int    `json:"type"`
	Token         string `json:"token"`
	GuildID       string `json:"guild_id"`
	ChannelID     string `json:"channel_id"`
	Channel       *struct {
		Type     int    `json:"type"`
		ParentID string `json:"parent_id"`
	} `json:"channel"`
	Member *struct {
		User  user     `json:"user"`
		Roles []string `json:"roles"`
	} `json:"member"`
	Data struct {
		Name    string   `json:"name"`
		Options []option `json:"options"`
	} `json:"data"`
}

// server is one linked panel server as a command sees it.
type server struct {
	inst    *store.Instance
	name    string
	players *int
}

// response is one interaction response.
type response struct {
	Type int `json:"type"`
	Data any `json:"data"`
}

// reply is a message response. Mentions are never parsed, so no text can ping anyone.
type reply struct {
	content   string
	ephemeral bool
}

// handle answers one interaction. Every answer goes back through Discord's callback endpoint.
func (b *Bot) handle(ctx context.Context, raw json.RawMessage) {
	var in interaction
	if err := json.Unmarshal(raw, &in); err != nil {
		slog.WarnContext(ctx, "read discord interaction", slog.Any("error", err))
		return
	}
	link, servers, err := b.linked(ctx, &in)
	if err != nil {
		slog.WarnContext(ctx, "resolve discord link", slog.Any("error", err))
	}
	switch in.Type {
	case interactionAutocomplete:
		offered := choices(servers, focused(in.Data.Options))
		if in.Data.Name == "shutdown" {
			offered = b.shutdownChoices(ctx, &in)
		}
		b.respond(ctx, &in, response{Type: responseAutocomplete, Data: map[string]any{"choices": offered}})
	case interactionCommand:
		var r reply
		switch {
		case err != nil:
			r = reply{tryLater, true}
		case in.GuildID == "":
			r = reply{"Use this command in a channel of a Discord server linked to the panel.", true}
		case link == nil:
			r = reply{"This channel isn't linked to any game servers.", true}
		case in.Data.Name == "status":
			r = statusReply(servers)
		case in.Data.Name == "start":
			r = b.start(ctx, &in, link, servers)
		case in.Data.Name == "shutdown":
			r = b.shutdown(ctx, &in)
		default:
			r = reply{"Unknown command.", true}
		}
		b.respond(ctx, &in, message(responseMessage, r))
	}
}

// linked resolves the interaction's channel to its link and that link's servers. A DM or an
// unlinked channel resolves to nothing.
func (b *Bot) linked(ctx context.Context, in *interaction) (*store.DiscordLink, []server, error) {
	if in.GuildID == "" {
		return nil, nil, nil
	}
	parent := ""
	if in.Channel != nil && slices.Contains(threadTypes, in.Channel.Type) {
		parent = in.Channel.ParentID
	}
	link, err := b.DB.DiscordLinkFor(ctx, in.GuildID, in.ChannelID, parent)
	if err != nil {
		return nil, nil, fmt.Errorf("find the link for channel %s: %w", in.ChannelID, err)
	}
	if link == nil {
		return nil, nil, nil
	}
	servers := make([]server, 0, len(link.InstanceIDs))
	for _, id := range link.InstanceIDs {
		inst, err := b.DB.InstanceByID(ctx, id)
		if err != nil {
			return nil, nil, fmt.Errorf("read linked server %s: %w", id, err)
		}
		if inst == nil {
			continue
		}
		name := inst.ServerName
		if name == "" {
			name = inst.Name
		}
		servers = append(servers, server{inst: inst, name: name, players: b.Players(inst.ID)})
	}
	slices.SortFunc(servers, func(a, b server) int { return strings.Compare(a.name, b.name) })
	return link, servers, nil
}

// statusReply lists the linked servers with their state and, when online, their players.
func statusReply(servers []server) reply {
	if len(servers) == 0 {
		return reply{"No game servers are linked to this channel.", false}
	}
	var sb strings.Builder
	for _, s := range servers {
		//nolint:exhaustive // every other state reads as offline
		switch instance.State(s.inst.State) {
		case instance.StateRunning:
			players := "players unknown"
			if s.players != nil {
				players = fmt.Sprintf("%d %s", *s.players, plural(*s.players, "player", "players"))
			}
			fmt.Fprintf(&sb, "🟢 **%s** — online, %s\n", s.name, players)
		case instance.StateStarting, instance.StateStopping:
			fmt.Fprintf(&sb, "🟡 **%s** — %s\n", s.name, s.inst.State)
		default:
			fmt.Fprintf(&sb, "⚪ **%s** — offline\n", s.name)
		}
	}
	return reply{strings.TrimSuffix(sb.String(), "\n"), false}
}

// start submits a start for the chosen linked server and follows it in the background.
func (b *Bot) start(ctx context.Context, in *interaction, link *store.DiscordLink, servers []server) reply {
	if !link.AllowStart {
		return reply{"Starting servers isn't allowed in this channel.", true}
	}
	target, r := pick(servers, stringOption(in.Data.Options, "server"))
	if target == nil {
		return r
	}
	inst, err := b.DB.InstanceByID(ctx, target.inst.ID)
	if err != nil || inst == nil {
		return reply{tryLater, true}
	}
	//nolint:exhaustive // the other states are checked against AllowedFrom below
	switch instance.State(inst.State) {
	case instance.StateRunning:
		return reply{fmt.Sprintf("**%s** is already online.", target.name), true}
	case instance.StateStarting:
		return reply{fmt.Sprintf("**%s** is already starting.", target.name), true}
	}
	if !slices.Contains(instance.AllowedFrom(jobs.KindStart), instance.State(inst.State)) {
		return reply{fmt.Sprintf("**%s** can't be started right now: it is %s.", target.name, inst.State), true}
	}
	if op, err := b.DB.OpenOperation(ctx, inst.ID); err != nil || op != nil || inst.ContainerID == nil {
		return reply{fmt.Sprintf("**%s** is busy with another change. Try again later.", target.name), true}
	}

	job, err := b.Starter.Submit(ctx, &control.StartSubmission{
		Instance: inst, ContainerID: *inst.ContainerID, Audit: startAudit(in, inst.ID),
	})
	var conflict *store.JobConflict
	switch {
	case errors.As(err, &conflict):
		return reply{fmt.Sprintf("**%s** is busy with another task. Try again in a few minutes.", target.name), true}
	case err != nil:
		slog.WarnContext(ctx, "discord start not submitted",
			slog.String("instance_id", inst.ID), slog.Any("error", err))
		return reply{fmt.Sprintf("**%s** could not be started.", target.name), true}
	}
	go b.follow(ctx, in, job.ID, target.name)
	return reply{fmt.Sprintf("Starting **%s**…", target.name), false}
}

// pick chooses the server a /start names: by id (an autocomplete choice) or by name, or the
// only linked server when none is named. A nil server comes with the reply explaining why.
func pick(servers []server, named string) (*server, reply) {
	if named == "" {
		if len(servers) == 1 {
			return &servers[0], reply{}
		}
		if len(servers) == 0 {
			return nil, reply{"No game servers are linked to this channel.", true}
		}
		return nil, reply{"Choose which server to start.", true}
	}
	for i := range servers {
		if servers[i].inst.ID == named || strings.EqualFold(servers[i].name, named) {
			return &servers[i], reply{}
		}
	}
	return nil, reply{"No server with that name is linked to this channel.", true}
}

// startAudit is the audit entry for a start a Discord user asked for.
func startAudit(in *interaction, instanceID string) *store.AuditEntry {
	return discordAudit(in, "instances.start", instanceID, nil)
}

// discordAudit is the audit entry for action taken by the Discord user behind in. detail adds
// to the fields naming the Discord user and channel.
func discordAudit(in *interaction, action, instanceID string, detail map[string]string) *store.AuditEntry {
	var who user
	if in.Member != nil {
		who = in.Member.User
	}
	fields := map[string]string{
		"via": "discord", "discord_user_id": who.ID, "guild_id": in.GuildID, "channel_id": in.ChannelID,
	}
	maps.Copy(fields, detail)
	raw, err := json.Marshal(fields)
	if err != nil {
		raw = nil
	}
	return &store.AuditEntry{
		InstanceID: instanceID, Action: action, ActorName: actorName(in), Detail: string(raw),
	}
}

// actorName names the Discord user behind in, as the audit log and the panel show them.
func actorName(in *interaction) string {
	var who user
	if in.Member != nil {
		who = in.Member.User
	}
	name := who.GlobalName
	if name == "" {
		name = who.Username
	}
	return fmt.Sprintf("Discord: %s (%s)", name, who.ID)
}

// follow polls the start job and edits the original reply once it finishes.
func (b *Bot) follow(ctx context.Context, in *interaction, jobID, name string) {
	ctx, cancel := context.WithTimeout(ctx, b.followFor)
	defer cancel()
	ticker := time.NewTicker(b.followTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		job, err := b.DB.JobByID(ctx, jobID)
		if err != nil {
			slog.WarnContext(ctx, "read discord start job", slog.String("job_id", jobID), slog.Any("error", err))
			continue
		}
		if job == nil {
			return
		}
		var text string
		switch job.Status {
		case jobs.StatusSucceeded:
			text = fmt.Sprintf("**%s** is online.", name)
		case jobs.StatusFailed:
			// The job's error names containers and paths, so it stays in the panel.
			text = fmt.Sprintf("**%s** failed to start. The panel's job history says why.", name)
		case jobs.StatusCancelled:
			text = fmt.Sprintf("Starting **%s** was cancelled.", name)
		default:
			continue
		}
		path := "/webhooks/" + in.ApplicationID + "/" + in.Token + "/messages/@original"
		body := map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}}
		if err := b.call(ctx, http.MethodPatch, path, "", body); err != nil {
			slog.WarnContext(ctx, "update discord start reply", slog.Any("error", err))
		}
		return
	}
}

// respond answers an interaction through its callback endpoint.
func (b *Bot) respond(ctx context.Context, in *interaction, body response) {
	path := "/interactions/" + in.ID + "/" + in.Token + "/callback"
	if err := b.call(ctx, http.MethodPost, path, "", body); err != nil {
		slog.WarnContext(ctx, "answer discord interaction", slog.Any("error", err))
	}
}

// message is a message response body for r.
func message(kind int, r reply) response {
	data := map[string]any{"content": r.content, "allowed_mentions": map[string]any{"parse": []string{}}}
	if r.ephemeral {
		data["flags"] = flagEphemeral
	}
	return response{Type: kind, Data: data}
}

// choices are the autocomplete suggestions: the linked servers whose name contains typed.
func choices(servers []server, typed string) []map[string]string {
	out := []map[string]string{}
	for _, s := range servers {
		if len(out) == maxChoices {
			break
		}
		if strings.Contains(strings.ToLower(s.name), strings.ToLower(typed)) {
			out = append(out, map[string]string{"name": s.name, "value": s.inst.ID})
		}
	}
	return out
}

// focused is the text typed into the option being autocompleted.
func focused(opts []option) string {
	for _, o := range opts {
		if o.Focused {
			var v string
			_ = json.Unmarshal(o.Value, &v)
			return v
		}
	}
	return ""
}

// stringOption is the named string option's value, empty when absent.
func stringOption(opts []option, name string) string {
	for _, o := range opts {
		if o.Name == name {
			var v string
			_ = json.Unmarshal(o.Value, &v)
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
