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
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// Names of the commands that submit jobs.
const (
	cmdStart   = "start"
	cmdRestart = "restart"
	cmdStop    = "stop"
	cmdBackup  = "backup"
	cmdUpdate  = "update"
)

// Interaction and response types the bot handles.
const (
	interactionCommand      = 2
	interactionAutocomplete = 4

	responseMessage      = 4
	responseDeferred     = 5
	responseAutocomplete = 8

	flagEphemeral = 64
	maxChoices    = 25
	// maxContent is the longest message content Discord accepts, in characters.
	maxContent = 2000
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
	// code is a crossplay server's join code for this boot, "" until it is logged.
	code string
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
		if err == nil && link != nil {
			switch in.Data.Name {
			case cmdStart:
				b.deferred(ctx, &in, func() (reply, *followed) { return b.start(ctx, &in, link, servers) })
				return
			case cmdRestart, cmdStop, cmdBackup, cmdUpdate:
				b.deferred(ctx, &in, func() (reply, *followed) { return b.admin(ctx, &in, servers) })
				return
			}
		}
		b.respond(ctx, &in, message(responseMessage, b.answerNow(ctx, &in, link, servers, err)))
	}
}

// answerNow is the reply to a command answered at once, or why it cannot be served: err is how
// resolving the channel's link failed, nil when it did not.
func (b *Bot) answerNow(
	ctx context.Context, in *interaction, link *store.DiscordLink, servers []server, err error,
) reply {
	switch {
	case err != nil:
		return reply{tryLater, true}
	case in.GuildID == "":
		return reply{"Use this command in a channel of a Discord server linked to the panel.", true}
	case link == nil:
		return reply{"This channel isn't linked to any game servers.", true}
	case in.Data.Name == "status":
		return statusReply(servers)
	case in.Data.Name == "join":
		return b.join(ctx, servers, stringOption(in.Data.Options, "server"))
	case in.Data.Name == "shutdown":
		return b.shutdown(ctx, in)
	}
	return reply{"Unknown command.", true}
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
		s := server{inst: inst, name: name, players: b.Players(inst.ID)}
		if inst.Crossplay && b.JoinCode != nil {
			s.code = b.JoinCode(inst.ID)
		}
		servers = append(servers, s)
	}
	slices.SortFunc(servers, func(a, b server) int { return strings.Compare(a.name, b.name) })
	return link, servers, nil
}

// statusReply lists the linked servers with their state and, when online, their players. A
// list longer than one message ends with how many servers it left out.
func statusReply(servers []server) reply {
	if len(servers) == 0 {
		return reply{"No game servers are linked to this channel.", false}
	}
	lines := make([]string, 0, len(servers))
	for _, s := range servers {
		lines = append(lines, statusLine(s))
	}
	return reply{fit(lines, "\n"), false}
}

// fit joins lines with sep into one message, dropping lines from the end and saying how many
// when they do not fit.
func fit(lines []string, sep string) string {
	text := strings.Join(lines, sep)
	for n := len(lines) - 1; utf8.RuneCountInString(text) > maxContent && n > 0; n-- {
		text = strings.Join(lines[:n], sep) + fmt.Sprintf("\n…and %d more", len(lines)-n)
	}
	return text
}

// statusLine is one server's line of /status.
func statusLine(s server) string {
	//nolint:exhaustive // every other state reads as offline
	switch instance.State(s.inst.State) {
	case instance.StateRunning:
		players := "players unknown"
		if s.players != nil {
			players = fmt.Sprintf("%d %s", *s.players, plural(*s.players, "player", "players"))
		}
		if s.code != "" {
			return fmt.Sprintf("🟢 **%s** — online, %s, join code %#q", s.name, players, s.code)
		}
		return fmt.Sprintf("🟢 **%s** — online, %s", s.name, players)
	case instance.StateStarting, instance.StateStopping:
		return fmt.Sprintf("🟡 **%s** — %s", s.name, s.inst.State)
	default:
		return fmt.Sprintf("⚪ **%s** — offline", s.name)
	}
}

// join says how to join the named linked server, or every linked server when none is named.
func (b *Bot) join(ctx context.Context, servers []server, named string) reply {
	if named != "" {
		target, r := pick(servers, named, "Choose which server to join.")
		if target == nil {
			return r
		}
		servers = []server{*target}
	}
	if len(servers) == 0 {
		return reply{"No game servers are linked to this channel.", true}
	}
	blocks := make([]string, 0, len(servers))
	for _, s := range servers {
		var info JoinInfo
		if b.Join != nil {
			info = b.Join(ctx, s.inst)
		}
		blocks = append(blocks, joinBlock(s, &info))
	}
	return reply{fit(blocks, "\n\n"), false}
}

// joinBlock is one server's part of /join: its join code or address, and its mod profile code.
func joinBlock(s server, info *JoinInfo) string {
	state := instance.State(s.inst.State)
	running := state == instance.StateRunning
	lines := []string{"**" + s.name + "**"}
	//nolint:exhaustive // every other state reads as offline
	switch state {
	case instance.StateRunning:
	case instance.StateStarting, instance.StateStopping:
		lines[0] += " — " + string(state)
	default:
		lines[0] += " — offline"
	}
	switch {
	case info.Code != "":
		lines = append(lines, "Join code: `"+info.Code+"`")
	case s.inst.Crossplay && running:
		lines = append(lines, "Join code: not available yet. Try again in a minute.")
	case s.inst.Crossplay:
		lines = append(lines, "Join code: shown once the server is online.")
	case info.Address != "":
		lines = append(lines, "Address: `"+info.Address+"`")
		if info.SteamURL != "" {
			lines = append(lines, "Join through Steam: "+info.SteamURL)
		}
	default:
		lines = append(lines, "No join address is set. A panel admin can set one in the server's settings.")
	}
	if info.ProfileCode != "" {
		lines = append(
			lines,
			fmt.Sprintf("Mod profile code for %s: %#q", registryName(info.ProfileRegistry), info.ProfileCode),
		)
	}
	return strings.Join(lines, "\n")
}

// registryName is a mod registry's id as players read it, with its first letter capitalised.
func registryName(id string) string {
	if id == "" {
		return "the mod manager"
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

// followed is a submitted job a reply follows, with what the reply says once the job ends.
type followed struct {
	jobID                   string
	done, failed, cancelled string
	// then, when set, runs once the job succeeded and returns the text to say instead of done.
	then func(ctx context.Context) string
}

// deferred answers a command at once with a deferred response, since its checks and claim can
// outlast Discord's three seconds, then replaces it with what submit replies and follows the job
// submit returns, if any.
func (b *Bot) deferred(ctx context.Context, in *interaction, submit func() (reply, *followed)) {
	b.respond(ctx, in, response{Type: responseDeferred, Data: map[string]any{}})
	r, f := submit()
	b.answer(ctx, in, r)
	if f != nil {
		b.tasks.Go(func() { b.follow(ctx, in, f) })
	}
}

// start submits a start for the chosen linked server, and returns the reply and the job to follow.
func (b *Bot) start(
	ctx context.Context, in *interaction, link *store.DiscordLink, servers []server,
) (reply, *followed) {
	if !link.AllowStart {
		return reply{"Starting servers isn't allowed in this channel.", true}, nil
	}
	target, r := pick(servers, stringOption(in.Data.Options, "server"), "Choose which server to start.")
	if target == nil {
		return r, nil
	}
	inst, refusal := b.startable(ctx, target)
	if inst == nil {
		return refusal, nil
	}

	job, err := b.Starter.Submit(ctx, &control.StartSubmission{
		Instance: inst, ContainerID: *inst.ContainerID, Audit: startAudit(in, inst.ID),
	})
	if err != nil {
		return submitRefusal(ctx, err, inst.ID, "**"+target.name+"**", "started"), nil
	}
	// The job's error names containers and paths, so it stays in the panel.
	return reply{fmt.Sprintf("Starting **%s**…", target.name), false}, &followed{
		jobID:     job.ID,
		done:      fmt.Sprintf("**%s** is online.", target.name),
		failed:    fmt.Sprintf("**%s** failed to start.", target.name) + jobHistory,
		cancelled: fmt.Sprintf("Starting **%s** was cancelled.", target.name),
	}
}

// submitRefusal is the reply to a job submission that failed: busy for a lock conflict, else
// that subject, a server's bolded name or a phrase naming it, could not be what past says, with
// the cause logged.
func submitRefusal(ctx context.Context, err error, instanceID, subject, past string) reply {
	var conflict *store.JobConflict
	if errors.As(err, &conflict) {
		return reply{subject + " is busy with another task. Try again in a few minutes.", true}
	}
	slog.WarnContext(ctx, "discord job not submitted", slog.String("instance_id", instanceID), slog.Any("error", err))
	return reply{fmt.Sprintf("%s could not be %s.", subject, past), true}
}

// startable re-reads target and returns it when it can start now, or nil with the reply that
// says why not.
func (b *Bot) startable(ctx context.Context, target *server) (*store.Instance, reply) {
	inst, err := b.DB.InstanceByID(ctx, target.inst.ID)
	if err != nil || inst == nil {
		return nil, reply{tryLater, true}
	}
	//nolint:exhaustive // the other states are checked against AllowedFrom below
	switch instance.State(inst.State) {
	case instance.StateRunning:
		return nil, reply{fmt.Sprintf("**%s** is already online.", target.name), true}
	case instance.StateStarting:
		return nil, reply{fmt.Sprintf("**%s** is already starting.", target.name), true}
	}
	if !slices.Contains(instance.AllowedFrom(jobs.KindStart), instance.State(inst.State)) {
		return nil, reply{
			fmt.Sprintf("**%s** can't be started right now: %s.", target.name, stateWords(instance.State(inst.State))),
			true,
		}
	}
	if op, err := b.DB.OpenOperation(ctx, inst.ID); err != nil || op != nil || inst.ContainerID == nil {
		return nil, reply{fmt.Sprintf("**%s** is busy with another change. Try again later.", target.name), true}
	}
	return inst, reply{}
}

// pick chooses the server a command names: by id (an autocomplete choice) or by name, or the
// only linked server when none is named. A nil server comes with the reply explaining why, which
// is prompt when the server must be chosen.
func pick(servers []server, named, prompt string) (*server, reply) {
	if named == "" {
		if len(servers) == 1 {
			return &servers[0], reply{}
		}
		if len(servers) == 0 {
			return nil, reply{"No game servers are linked to this channel.", true}
		}
		return nil, reply{prompt, true}
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

// follow waits for f's job to end and edits the reply to say how it ended.
func (b *Bot) follow(ctx context.Context, in *interaction, f *followed) {
	ctx, cancel := context.WithTimeout(ctx, b.followFor)
	defer cancel()
	status, ok := b.await(ctx, f.jobID)
	if !ok {
		return
	}
	text := f.cancelled
	switch status {
	case jobs.StatusSucceeded:
		text = f.done
		if f.then != nil {
			text = f.then(ctx)
		}
	case jobs.StatusFailed:
		text = f.failed
	}
	// The follow's own time may be spent by then, so the edit gets a little of its own.
	answerCtx, cancelAnswer := context.WithTimeout(context.WithoutCancel(ctx), answerTimeout)
	defer cancelAnswer()
	b.answer(answerCtx, in, reply{content: text})
}

// answerTimeout bounds the edit that ends a follow.
const answerTimeout = 10 * time.Second

// await polls job id until it ends and returns its status. It reports false when ctx ends first
// or the job is gone.
func (b *Bot) await(ctx context.Context, id string) (string, bool) {
	ticker := time.NewTicker(b.followTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-ticker.C:
		}
		job, err := b.DB.JobByID(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "read discord job", slog.String("job_id", id), slog.Any("error", err))
			continue
		}
		if job == nil {
			return "", false
		}
		switch job.Status {
		case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled:
			return job.Status, true
		}
	}
}

// respond answers an interaction through its callback endpoint.
func (b *Bot) respond(ctx context.Context, in *interaction, body response) {
	path := "/interactions/" + in.ID + "/" + in.Token + "/callback"
	if err := b.call(ctx, http.MethodPost, "/interactions/{id}/{token}/callback", path, "", body); err != nil {
		slog.WarnContext(ctx, "answer discord interaction", slog.Any("error", err))
	}
}

// answer replaces a deferred or earlier response with r. A deferred response is public, so an
// ephemeral r deletes it and is sent as an ephemeral follow-up instead.
func (b *Bot) answer(ctx context.Context, in *interaction, r reply) {
	webhook := "/webhooks/" + in.ApplicationID + "/" + in.Token
	const route = "/webhooks/{app}/{token}"
	data := message(responseMessage, r).Data
	var err error
	if r.ephemeral {
		err = b.call(ctx, http.MethodDelete, route+"/messages/@original", webhook+"/messages/@original", "", nil)
		if err == nil {
			err = b.call(ctx, http.MethodPost, route, webhook, "", data)
		}
	} else {
		err = b.call(ctx, http.MethodPatch, route+"/messages/@original", webhook+"/messages/@original", "", data)
	}
	if err != nil {
		slog.WarnContext(ctx, "update discord reply", slog.Any("error", err))
	}
}

// message is a message response body for r, its content cut to the longest Discord accepts.
func message(kind int, r reply) response {
	content := r.content
	if utf8.RuneCountInString(content) > maxContent {
		content = string([]rune(content)[:maxContent-1]) + "…"
	}
	data := map[string]any{"content": content, "allowed_mentions": notify.NoMentions}
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

// focused is the text typed into the option being autocompleted, which may sit inside a
// subcommand.
func focused(opts []option) string {
	for _, o := range opts {
		if o.Focused {
			var v string
			_ = json.Unmarshal(o.Value, &v)
			return v
		}
		if v := focused(o.Options); v != "" {
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

// intOption is the named integer option's value, and false when it is absent.
func intOption(opts []option, name string) (int, bool) {
	for _, o := range opts {
		var v int
		if o.Name == name && json.Unmarshal(o.Value, &v) == nil {
			return v, true
		}
	}
	return 0, false
}

// boolOption is the named boolean option's value, false when absent.
func boolOption(opts []option, name string) bool {
	for _, o := range opts {
		var v bool
		if o.Name == name && json.Unmarshal(o.Value, &v) == nil {
			return v
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
