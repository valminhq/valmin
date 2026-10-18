package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// restAttempts bounds how often one call is retried after Discord rate-limits it.
const restAttempts = 3

// appCommand is a slash command or one of its options. Type 1 is a chat command, or a
// subcommand among options; 3 is a string option.
type appCommand struct {
	Type         int          `json:"type"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Required     bool         `json:"required,omitempty"`
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
	{
		Type: 1, Name: "shutdown", Description: "Plan a power cut: every game server stops before it (bot admins)",
		Options: []appCommand{
			{Type: 1, Name: "add", Description: "Plan a power cut", Options: []appCommand{
				{
					Type:        3,
					Name:        "time",
					Description: "When the power goes off: 14:00 or 2026-10-09 14:00",
					Required:    true,
				},
			}},
			{Type: 1, Name: "list", Description: "List the planned power cuts"},
			{Type: 1, Name: "cancel", Description: "Cancel a planned power cut", Options: []appCommand{{
				Type: 3, Name: "time", Description: "The power cut to cancel", Required: true, Autocomplete: true,
			}}},
		},
	},
}

// call sends one request to the Discord API. token, when set, authenticates as the bot; the
// interaction endpoints authenticate by the interaction token in the path instead, so errors
// name route, the path's template, and never path itself.
func (b *Bot) call(ctx context.Context, method, route, path, token string, body any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode discord request: %w", err)
		}
	}
	for attempt := 1; ; attempt++ {
		status, raw, err := b.send(ctx, method, path, token, payload)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, route, err)
		}
		if status < 300 {
			return nil
		}
		if status != http.StatusTooManyRequests || attempt == restAttempts {
			return fmt.Errorf("%s %s: discord answered %d: %s", method, route, status, raw)
		}
		var limit struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(raw, &limit)
		wait := time.NewTimer(time.Duration(max(limit.RetryAfter, 0.1) * float64(time.Second)))
		select {
		case <-ctx.Done():
			wait.Stop()
			return fmt.Errorf("%s %s: %w", method, route, ctx.Err())
		case <-wait.C:
		}
	}
}

// send makes one attempt and returns Discord's status and up to 64 KiB of its answer. A
// transport error is returned without the URL it was for, which can carry a token.
func (b *Bot) send(
	ctx context.Context, method, path, token string, payload []byte,
) (status int, answer []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, method, b.apiBase+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, errors.New("build discord request")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/valminhq/valmin, 1)")
	if token != "" {
		req.Header.Set("Authorization", "Bot "+token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, nil, err
	}
	answer, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, answer, nil
}

// registerCommands puts the commands in every linked Discord server and clears them in every
// other server the bot is in, so a removed link takes its commands with it.
func (b *Bot) registerCommands(ctx context.Context, applicationID, token string, joined []string) {
	linked, err := b.DB.DiscordGuilds(ctx)
	if err != nil {
		slog.WarnContext(ctx, "list linked discord servers", slog.Any("error", err))
		return
	}
	set := make(map[string][]appCommand, len(linked)+len(joined))
	for _, guild := range joined {
		set[guild] = []appCommand{}
	}
	for _, guild := range linked {
		set[guild] = commands
	}
	for guild, cmds := range set {
		path := "/applications/" + applicationID + "/guilds/" + guild + "/commands"
		if err := b.call(
			ctx,
			http.MethodPut,
			"/applications/{app}/guilds/{guild}/commands",
			path,
			token,
			cmds,
		); err != nil {
			slog.WarnContext(ctx, "register discord commands",
				slog.String("guild_id", guild), slog.Any("error", err))
		}
	}
}
