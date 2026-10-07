package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// restAttempts bounds how often one call is retried after Discord rate-limits it.
const restAttempts = 3

// appCommand is a slash command or one of its options. Type 1 is a chat command, 3 a string
// option.
type appCommand struct {
	Type         int          `json:"type"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Autocomplete bool         `json:"autocomplete,omitempty"`
	Options      []appCommand `json:"options,omitempty"`
}

// commands are the slash commands registered in every linked Discord server.
var commands = []appCommand{
	{Type: 1, Name: "status", Description: "Show the game servers linked to this channel"},
	{
		Type: 1, Name: "start", Description: "Start a game server linked to this channel",
		Options: []appCommand{{Type: 3, Name: "server", Description: "The server to start", Autocomplete: true}},
	},
}

// call sends one request to the Discord API. token, when set, authenticates as the bot; the
// interaction endpoints authenticate by the interaction token in the path instead.
func (b *Bot) call(ctx context.Context, method, path, token string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode discord request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, b.apiBase+path, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build discord request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/valminhq/valmin, 1)")
		if token != "" {
			req.Header.Set("Authorization", "Bot "+token)
		}
		resp, err := b.client.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == restAttempts {
			return fmt.Errorf("%s %s: discord answered %d: %s", method, path, resp.StatusCode, raw)
		}
		var limit struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(raw, &limit)
		wait := time.NewTimer(time.Duration(max(limit.RetryAfter, 0.1) * float64(time.Second)))
		select {
		case <-ctx.Done():
			wait.Stop()
			return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
		case <-wait.C:
		}
	}
}

// registerCommands puts the commands in every linked Discord server. Only linked servers get
// them, so the commands appear nowhere else.
func (b *Bot) registerCommands(ctx context.Context, applicationID, token string) {
	guilds, err := b.DB.DiscordGuilds(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list linked discord servers", slog.Any("error", err))
		return
	}
	for _, guild := range guilds {
		path := "/applications/" + applicationID + "/guilds/" + guild + "/commands"
		if err := b.call(ctx, http.MethodPut, path, token, commands); err != nil {
			slog.WarnContext(ctx, "register discord commands",
				slog.String("guild_id", guild), slog.Any("error", err))
		}
	}
}
