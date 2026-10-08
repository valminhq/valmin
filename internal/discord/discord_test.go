package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/store/storetest"
)

const testToken = "bot-token"

// request is one call the fake Discord API received.
type request struct {
	method, path, auth string
	body               map[string]any
}

// fakeAPI records every call and answers 204.
type fakeAPI struct {
	mu   sync.Mutex
	got  []request
	srv  *httptest.Server
	seen chan request
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{seen: make(chan request, 64)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &req.body)
		f.mu.Lock()
		f.got = append(f.got, req)
		f.mu.Unlock()
		f.seen <- req
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// next waits for the next call matching method and a path prefix.
func (f *fakeAPI) next(t *testing.T, method, prefix string) request {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case r := <-f.seen:
			if r.method == method && strings.HasPrefix(r.path, prefix) {
				return r
			}
		case <-timeout:
			t.Fatalf("no %s %s* call", method, prefix)
		}
	}
}

// fakeStarter records start submissions and answers with job or err.
type fakeStarter struct {
	mu   sync.Mutex
	got  []*control.StartSubmission
	job  *store.Job
	fail error
}

func (s *fakeStarter) Submit(_ context.Context, in *control.StartSubmission) (*store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, in)
	if s.fail != nil {
		return nil, s.fail
	}
	return s.job, nil
}

// world is a bot against a fresh database, a fake API and a fake starter.
type world struct {
	bot     *Bot
	db      *store.DB
	api     *fakeAPI
	starter *fakeStarter
}

func newWorld(t *testing.T, links []store.DiscordLink) *world {
	t.Helper()
	db := storetest.Open(t)
	keeper, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct{ id, name, state string }{
		{"inst-a", "Alpha", "stopped"}, {"inst-b", "Bravo", "running"}, {"inst-x", "Xray", "stopped"},
	} {
		if _, err := db.Writer.ExecContext(t.Context(), `INSERT INTO instances (
			id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'World', 'env', ?, ?, ?)`,
			s.id, s.id, s.state, "c-"+s.id, "/srv/"+s.id, 2456+len(s.id)*10+int(s.id[5]), s.name,
			"cp-"+s.id, store.Now(), store.Now()); err != nil {
			t.Fatal(err)
		}
	}
	envelope, err := keeper.Encrypt(crypto.PurposeDiscordToken, crypto.DiscordTokenLocation("bot"), []byte(testToken))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveDiscordBot(t.Context(), &store.DiscordBot{ID: "bot", Token: envelope, Enabled: true},
		links, "", nil); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI(t)
	starter := &fakeStarter{job: &store.Job{ID: "job-1"}}
	players := map[string]int{"inst-b": 2}
	bot := New(db, keeper, starter, func(id string) *int {
		if n, ok := players[id]; ok {
			return &n
		}
		return nil
	})
	bot.apiBase, bot.client = api.srv.URL, api.srv.Client()
	bot.followTick, bot.followFor = 10*time.Millisecond, 2*time.Second
	return &world{bot: bot, db: db, api: api, starter: starter}
}

var testLinks = []store.DiscordLink{
	{GuildID: "g1", AllowStart: true, InstanceIDs: []string{"inst-a", "inst-b"}},
	{GuildID: "g1", ChannelID: "ch-status", InstanceIDs: []string{"inst-a"}},
	{GuildID: "g1", ChannelID: "ch-parent", AllowStart: true, InstanceIDs: []string{"inst-b"}},
}

// command builds an INTERACTION_CREATE for a slash command.
func command(guild, channel, parent, name, server string) json.RawMessage {
	in := map[string]any{
		"id": "i-1", "application_id": "app-1", "type": interactionCommand, "token": "itok",
		"guild_id": guild, "channel_id": channel,
		"member": map[string]any{"user": map[string]any{"id": "u-1", "username": "den", "global_name": "Den"}},
		"data":   map[string]any{"name": name},
	}
	if parent != "" {
		in["channel"] = map[string]any{"type": 11, "parent_id": parent}
	}
	if server != "" {
		in["data"] = map[string]any{"name": name, "options": []map[string]any{{"name": "server", "value": server}}}
	}
	raw, _ := json.Marshal(in)
	return raw
}

// TestCommands asserts what each command answers in each kind of channel, that no answer can
// mention anyone, and that only a permitted /start reaches the starter.
func TestCommands(t *testing.T) {
	tests := []struct {
		name      string
		in        json.RawMessage
		fail      error
		want      string
		ephemeral bool
		submitted bool
	}{
		{"a DM", command("", "dm", "", "status", ""), nil, "Use this command in a channel", true, false},
		{"an unlinked Discord server", command("g2", "c", "", "status", ""), nil, "isn't linked", true, false},
		{
			"status lists the linked servers", command("g1", "general", "", "status", ""), nil,
			"⚪ **Alpha** — offline\n🟢 **Bravo** — online, 2 players", false, false,
		},
		{
			"status in a narrower channel", command("g1", "ch-status", "", "status", ""), nil,
			"⚪ **Alpha** — offline", false, false,
		},
		{
			"start where it is not allowed", command("g1", "ch-status", "", "start", ""), nil,
			"isn't allowed in this channel", true, false,
		},
		{
			"a thread inherits its parent's link", command("g1", "thread", "ch-parent", "start", ""), nil,
			"**Bravo** is already online.", true, false,
		},
		{
			"a server outside the link", command("g1", "general", "", "start", "Xray"), nil,
			"No server with that name", true, false,
		},
		{
			"two servers and none named", command("g1", "general", "", "start", ""), nil,
			"Choose which server", true, false,
		},
		{
			"a busy server", command("g1", "general", "", "start", "alpha"), &store.JobConflict{JobID: "j"},
			"busy with another task", true, true,
		},
		{"a start by name", command("g1", "general", "", "start", "alpha"), nil, "Starting **Alpha**…", false, true},
		{"a start by id", command("g1", "general", "", "start", "inst-a"), nil, "Starting **Alpha**…", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, testLinks)
			w.starter.fail = tt.fail
			w.bot.handle(t.Context(), tt.in)

			got := w.api.next(t, http.MethodPost, "/interactions/i-1/itok/callback")
			data, _ := got.body["data"].(map[string]any)
			content, _ := data["content"].(string)
			if !strings.Contains(content, tt.want) {
				t.Errorf("content = %q, want it to contain %q", content, tt.want)
			}
			if _, ephemeral := data["flags"]; ephemeral != tt.ephemeral {
				t.Errorf("ephemeral = %v, want %v", ephemeral, tt.ephemeral)
			}
			mentions, _ := data["allowed_mentions"].(map[string]any)
			if parse, ok := mentions["parse"].([]any); !ok || len(parse) != 0 {
				t.Errorf("allowed_mentions = %v, want parse []", data["allowed_mentions"])
			}
			if got.auth != "" {
				t.Errorf("an interaction callback carried the bot token")
			}
			if submitted := len(w.starter.got) > 0; submitted != tt.submitted {
				t.Fatalf("submitted = %v, want %v", submitted, tt.submitted)
			}
		})
	}
}

// TestAStartIsAuditedAsTheDiscordUser asserts the submission names no panel user, carries the
// Discord user in its audit entry, and that the reply is edited once the job finishes.
func TestAStartIsAuditedAsTheDiscordUser(t *testing.T) {
	w := newWorld(t, testLinks)
	if _, err := w.db.Writer.ExecContext(t.Context(), `INSERT INTO job_runs
		(id, kind, status, lock_key, instance_id, instance_name, created_at)
		VALUES ('job-1', 'start', 'succeeded', 'k', 'inst-a', 'inst-a', ?)`,
		store.Now()); err != nil {
		t.Fatal(err)
	}
	w.bot.handle(t.Context(), command("g1", "general", "", "start", "Alpha"))

	sub := w.starter.got[0]
	if sub.RequestedBy != "" || sub.Instance.ID != "inst-a" || sub.ContainerID != "c-inst-a" {
		t.Errorf("submission = %+v", sub)
	}
	if sub.Audit == nil || sub.Audit.ActorName != "Discord: Den (u-1)" || sub.Audit.Action != "instances.start" ||
		!strings.Contains(sub.Audit.Detail, `"discord_user_id":"u-1"`) {
		t.Errorf("audit = %+v", sub.Audit)
	}
	edit := w.api.next(t, http.MethodPatch, "/webhooks/app-1/itok/messages/@original")
	if edit.body["content"] != "**Alpha** is online." {
		t.Errorf("edited reply = %v", edit.body["content"])
	}
}

// TestAutocompleteOffersOnlyLinkedServers asserts suggestions come from the channel's link.
func TestAutocompleteOffersOnlyLinkedServers(t *testing.T) {
	w := newWorld(t, testLinks)
	raw, _ := json.Marshal(map[string]any{
		"id": "i-1", "application_id": "app-1", "type": interactionAutocomplete, "token": "itok",
		"guild_id": "g1", "channel_id": "general",
		"data": map[string]any{"name": "start", "options": []map[string]any{
			{"name": "server", "value": "a", "focused": true},
		}},
	})
	w.bot.handle(t.Context(), raw)

	got := w.api.next(t, http.MethodPost, "/interactions/i-1/itok/callback")
	data, _ := got.body["data"].(map[string]any)
	choices, _ := data["choices"].([]any)
	names := make([]string, 0, len(choices))
	for _, c := range choices {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "Alpha,Bravo" {
		t.Errorf("choices = %v, want Alpha,Bravo", names)
	}
}

// gateway is a fake Discord Gateway. script runs once per connection.
func gateway(
	t *testing.T,
	script func(ctx context.Context, conn *websocket.Conn, n int),
) (url string, conns func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		mu.Lock()
		n++
		this := n
		mu.Unlock()
		script(r.Context(), conn, this)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// run starts the bot and stops it when the test ends.
func run(t *testing.T, b *Bot) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheBotIdentifiesHeartbeatsAndRegistersCommands asserts identifying sends the token with
// no intents, heartbeats are answered, and commands go to each linked Discord server.
func TestTheBotIdentifiesHeartbeatsAndRegistersCommands(t *testing.T) {
	w := newWorld(t, append(testLinks, store.DiscordLink{GuildID: "g9"}))
	identified := make(chan map[string]any, 1)
	beats := make(chan struct{}, 8)
	url, _ := gateway(t, func(ctx context.Context, conn *websocket.Conn, _ int) {
		_ = wsjson.Write(ctx, conn, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 30}})
		var id frame
		if wsjson.Read(ctx, conn, &id) != nil {
			return
		}
		var d map[string]any
		_ = json.Unmarshal(id.D, &d)
		identified <- d
		_ = wsjson.Write(ctx, conn, map[string]any{"op": opDispatch, "t": "READY", "s": 1, "d": map[string]any{
			"user": map[string]any{"username": "ValminBot"}, "application": map[string]any{"id": "app-1"},
		}})
		for {
			var f frame
			if wsjson.Read(ctx, conn, &f) != nil {
				return
			}
			if f.Op == opHeartbeat {
				select {
				case beats <- struct{}{}:
				default:
				}
				_ = wsjson.Write(ctx, conn, map[string]any{"op": opHeartbeatACK})
			}
		}
	})
	w.bot.gatewayURL = url
	run(t, w.bot)

	d := <-identified
	if d["token"] != testToken || d["intents"] != float64(0) {
		t.Errorf("identify = %v", d)
	}
	registered := make([]string, 0, 2)
	for range 2 {
		reg := w.api.next(t, http.MethodPut, "/applications/app-1/guilds/")
		if reg.auth != "Bot "+testToken {
			t.Errorf("registration auth = %q", reg.auth)
		}
		registered = append(registered, reg.path)
	}
	slices.Sort(registered)
	if want := []string{
		"/applications/app-1/guilds/g1/commands",
		"/applications/app-1/guilds/g9/commands",
	}; !slices.Equal(
		registered,
		want,
	) {
		t.Errorf("registered = %v, want %v", registered, want)
	}
	waitFor(t, "two heartbeats", func() bool { return len(beats) >= 2 })
	if s := w.bot.Status(); s.State != StateConnected || s.BotName != "ValminBot" || s.ApplicationID != "app-1" {
		t.Errorf("status = %+v", s)
	}
}

// TestARejectedTokenStopsTheBot asserts a 4004 close leaves the bot failed without reconnecting.
func TestARejectedTokenStopsTheBot(t *testing.T) {
	w := newWorld(t, testLinks)
	url, conns := gateway(t, func(ctx context.Context, conn *websocket.Conn, _ int) {
		_ = wsjson.Write(ctx, conn, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 40000}})
		var f frame
		_ = wsjson.Read(ctx, conn, &f)
		_ = conn.Close(4004, "Authentication failed.")
	})
	w.bot.gatewayURL = url
	run(t, w.bot)

	waitFor(t, "the failed state", func() bool { return w.bot.Status().State == StateFailed })
	if !strings.Contains(w.bot.Status().Error, "rejected the bot token") {
		t.Errorf("error = %q", w.bot.Status().Error)
	}
	time.Sleep(200 * time.Millisecond)
	if n := conns(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}

// TestAReconnectRequestReconnects asserts op 7 opens a fresh connection at once.
func TestAReconnectRequestReconnects(t *testing.T) {
	w := newWorld(t, testLinks)
	url, conns := gateway(t, func(ctx context.Context, conn *websocket.Conn, n int) {
		_ = wsjson.Write(ctx, conn, map[string]any{"op": opHello, "d": map[string]any{"heartbeat_interval": 40000}})
		var f frame
		_ = wsjson.Read(ctx, conn, &f)
		if n == 1 {
			_ = wsjson.Write(ctx, conn, map[string]any{"op": opReconnect})
		}
		<-ctx.Done()
	})
	w.bot.gatewayURL = url
	run(t, w.bot)
	waitFor(t, "a second connection", func() bool { return conns() >= 2 })
}

// TestADisabledBotDoesNotConnect asserts a disabled bot stays off until Reload finds it enabled.
func TestADisabledBotDoesNotConnect(t *testing.T) {
	w := newWorld(t, testLinks)
	url, conns := gateway(t, func(ctx context.Context, conn *websocket.Conn, _ int) { <-ctx.Done() })
	w.bot.gatewayURL = url
	if err := w.db.DisableDiscordBot(t.Context()); err != nil {
		t.Fatal(err)
	}
	run(t, w.bot)
	time.Sleep(100 * time.Millisecond)
	if n := conns(); n != 0 || w.bot.Status().State != StateDisabled {
		t.Fatalf("connections = %d, status = %+v", n, w.bot.Status())
	}
	if _, err := w.db.Writer.ExecContext(t.Context(), `UPDATE discord_bot SET enabled = TRUE`); err != nil {
		t.Fatal(err)
	}
	w.bot.Reload()
	waitFor(t, "a connection after Reload", func() bool { return conns() == 1 })
}

// TestAFailedStartKeepsItsErrorInThePanel asserts the edited reply never carries the job's
// error, which names containers and paths.
func TestAFailedStartKeepsItsErrorInThePanel(t *testing.T) {
	w := newWorld(t, testLinks)
	if _, err := w.db.Writer.ExecContext(t.Context(), `INSERT INTO job_runs
		(id, kind, status, lock_key, instance_id, instance_name, error, created_at)
		VALUES ('job-1', 'start', 'failed', 'k', 'inst-a', 'inst-a', 'start container c-inst-a: /srv/secret', ?)`,
		store.Now()); err != nil {
		t.Fatal(err)
	}
	w.bot.handle(t.Context(), command("g1", "general", "", "start", "Alpha"))

	edit := w.api.next(t, http.MethodPatch, "/webhooks/app-1/itok/messages/@original")
	content, _ := edit.body["content"].(string)
	if !strings.Contains(content, "failed to start") || strings.Contains(content, "/srv/secret") {
		t.Errorf("edited reply = %q", content)
	}
}

// shutdownCommand builds a /shutdown interaction for subcommand sub from member u-1 holding
// roles. text, when set, is the time option.
func shutdownCommand(interactionType int, sub, text string, roles ...string) json.RawMessage {
	option := map[string]any{"name": sub, "type": 1}
	if text != "" {
		option["options"] = []map[string]any{{"name": "time", "value": text, "focused": true}}
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "i-1", "application_id": "app-1", "type": interactionType, "token": "itok",
		"guild_id": "g1", "channel_id": "general",
		"member": map[string]any{
			"user":  map[string]any{"id": "u-1", "username": "den", "global_name": "Den"},
			"roles": roles,
		},
		"data": map[string]any{"name": "shutdown", "options": []map[string]any{option}},
	})
	return raw
}

// answer runs one interaction and returns the reply's content and whether it was ephemeral.
func (w *world) answer(t *testing.T, in json.RawMessage) (string, bool) {
	t.Helper()
	w.bot.handle(t.Context(), in)
	got := w.api.next(t, http.MethodPost, "/interactions/i-1/itok/callback")
	data, _ := got.body["data"].(map[string]any)
	content, _ := data["content"].(string)
	_, ephemeral := data["flags"]
	return content, ephemeral
}

// withAdmins makes ids the bot's admins and reads typed times in Europe/Kyiv.
func (w *world) withAdmins(t *testing.T, ids string) {
	t.Helper()
	if _, err := w.db.Writer.ExecContext(t.Context(),
		`UPDATE discord_bot SET admin_ids = ?, timezone = 'Europe/Kyiv'`, ids); err != nil {
		t.Fatal(err)
	}
}

// TestOnlyBotAdminsPlanPowerCuts asserts a member listed by user id or by role may plan a power
// cut, and anyone else is refused without one being stored.
func TestOnlyBotAdminsPlanPowerCuts(t *testing.T) {
	tests := []struct {
		name, admins string
		roles        []string
		want         string
		stored       int
	}{
		{"no admins", `[]`, nil, "Only the bot's admins", 0},
		{"another member", `["u-2","r-admin"]`, []string{"r-other"}, "Only the bot's admins", 0},
		{"listed by user id", `["u-1"]`, nil, "Power cut planned", 1},
		{"listed by role", `["r-admin"]`, []string{"r-other", "r-admin"}, "Power cut planned", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, testLinks)
			w.withAdmins(t, tt.admins)
			content, _ := w.answer(t, shutdownCommand(interactionCommand, "add", "2099-01-01 14:00", tt.roles...))
			if !strings.Contains(content, tt.want) {
				t.Errorf("content = %q, want it to contain %q", content, tt.want)
			}
			planned, err := w.db.UpcomingShutdowns(t.Context(), time.Now())
			if err != nil || len(planned) != tt.stored {
				t.Fatalf("planned = %+v, %v; want %d", planned, err, tt.stored)
			}
		})
	}
}

// TestAPowerCutRoundTripsThroughDiscord asserts a planned power cut is read in the bot's zone,
// recorded as the Discord user's, listed, offered for cancelling, and cancelled.
func TestAPowerCutRoundTripsThroughDiscord(t *testing.T) {
	w := newWorld(t, testLinks)
	w.withAdmins(t, `["r-admin"]`)
	at := time.Date(2099, 1, 1, 12, 0, 0, 0, time.UTC)

	content, ephemeral := w.answer(t, shutdownCommand(interactionCommand, "add", "2099-01-01 14:00", "r-admin"))
	if want := fmt.Sprintf("<t:%d:F>", at.Unix()); !strings.Contains(content, want) || ephemeral {
		t.Fatalf("add = %q (ephemeral %v), want a public reply naming %s", content, ephemeral, want)
	}
	planned, err := w.db.UpcomingShutdowns(t.Context(), time.Now())
	if err != nil || len(planned) != 1 || !planned[0].PowerOffAt.Equal(at) ||
		planned[0].CreatedByName != "Discord: Den (u-1)" || planned[0].CreatedBy != nil {
		t.Fatalf("planned = %+v, %v", planned, err)
	}

	if content, _ := w.answer(t, shutdownCommand(interactionCommand, "add", "yesterday", "r-admin")); !strings.Contains(
		content, "Type the time") {
		t.Errorf("unreadable time = %q", content)
	}
	if content, _ := w.answer(t, shutdownCommand(interactionCommand, "list", "", "r-admin")); !strings.Contains(
		content, fmt.Sprintf("<t:%d:F>", at.Unix())) {
		t.Errorf("list = %q", content)
	}

	w.bot.handle(t.Context(), shutdownCommand(interactionAutocomplete, "cancel", "", "r-admin"))
	got := w.api.next(t, http.MethodPost, "/interactions/i-1/itok/callback")
	data, _ := got.body["data"].(map[string]any)
	choices, _ := data["choices"].([]any)
	if len(choices) != 1 || choices[0].(map[string]any)["value"] != planned[0].ID ||
		choices[0].(map[string]any)["name"] != "Thu 1 Jan 14:00 EET" {
		t.Fatalf("choices = %v", choices)
	}

	if content, _ := w.answer(
		t,
		shutdownCommand(interactionCommand, "cancel", planned[0].ID, "r-admin"),
	); !strings.Contains(
		content,
		"is cancelled",
	) {
		t.Errorf("cancel = %q", content)
	}
	if left, _ := w.db.UpcomingShutdowns(t.Context(), time.Now()); len(left) != 0 {
		t.Errorf("after cancel = %+v", left)
	}
}
