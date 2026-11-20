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

// Option types a command uses.
const (
	optionSubcommand = 1
	optionString     = 3
	optionInteger    = 4
	optionBoolean    = 5
)

// appCommand is a slash command (type 1), or one of its options: a subcommand or a value.
type appCommand struct {
	Type         int          `json:"type"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Required     bool         `json:"required,omitempty"`
	Autocomplete bool         `json:"autocomplete,omitempty"`
	Choices      []choice     `json:"choices,omitempty"`
	MinValue     *int         `json:"min_value,omitempty"`
	MaxValue     *int         `json:"max_value,omitempty"`
	Options      []appCommand `json:"options,omitempty"`
}

// choice is one fixed value a string option offers.
type choice struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// serverOption is the option naming the linked server a command acts on.
func serverOption(description string) appCommand {
	return appCommand{Type: optionString, Name: "server", Description: description, Autocomplete: true}
}

// waitOption is the option of /restart, /stop and /backup saying how long to wait for players
// to leave a running server.
func waitOption() appCommand {
	low, high := 0, maxWaitMinutes
	return appCommand{
		Type: optionInteger,
		Name: "wait",
		Description: fmt.Sprintf(
			"Minutes to wait for players to leave after warning them (default %d)",
			defaultWaitMinutes,
		),
		MinValue: &low,
		MaxValue: &high,
	}
}

// commands are the slash commands registered in every linked Discord server.
var commands = []appCommand{
	{Type: 1, Name: "status", Description: "Show the game servers linked to this channel"},
	{
		Type: 1, Name: "join", Description: "Show how to join the game servers linked to this channel",
		Options: []appCommand{serverOption("The server to join")},
	},
	{
		Type: 1, Name: cmdStart, Description: "Start a game server linked to this channel",
		Options: []appCommand{serverOption("The server to start")},
	},
	{
		Type: 1, Name: cmdRestart, Description: "Restart a game server once players have left (bot admins)",
		Options: []appCommand{serverOption("The server to restart"), waitOption()},
	},
	{
		Type: 1, Name: cmdStop, Description: "Stop a game server once players have left (bot admins)",
		Options: []appCommand{serverOption("The server to stop"), waitOption()},
	},
	{
		Type:        1,
		Name:        cmdBackup,
		Description: "Back up a game server's world; a running server restarts for it (bot admins)",
		Options:     []appCommand{serverOption("The server to back up"), waitOption()},
	},
	{
		Type:        1,
		Name:        cmdUpdate,
		Description: "Test updates on a copy of a game server, then apply or discard them (bot admins)",
		Options: []appCommand{
			{
				Type:        optionSubcommand,
				Name:        "test",
				Description: "Try the game or mod updates on a copy",
				Options: []appCommand{
					serverOption("The server to test updates on"),
					{
						Type:        optionString,
						Name:        "what",
						Description: "What to update (default: game and mods)",
						Choices: []choice{
							{
								Name:  "Game and mods",
								Value: "both",
							},
							{Name: "Game", Value: "game"},
							{Name: "Mods", Value: "mods"},
						},
					},
					{
						Type: optionBoolean, Name: "apply",
						Description: "Apply the updates if every mod loaded and nobody is on",
					},
				},
			},
			{
				Type:        optionSubcommand,
				Name:        "apply",
				Description: "Install the tested updates on the server",
				Options: []appCommand{
					serverOption("The server to update"),
				},
			},
			{Type: optionSubcommand, Name: "discard", Description: "Delete the update test copy", Options: []appCommand{
				serverOption("The server whose test copy to delete"),
			}},
		},
	},
	{
		Type: 1, Name: "shutdown", Description: "Plan a power cut: every game server stops before it (bot admins)",
		Options: []appCommand{
			{Type: optionSubcommand, Name: "add", Description: "Plan a power cut", Options: []appCommand{
				{
					Type:        optionString,
					Name:        "time",
					Description: "When the power goes off: 14:00 or 2026-10-09 14:00",
					Required:    true,
				},
			}},
			{Type: optionSubcommand, Name: "list", Description: "List the planned power cuts"},
			{Type: optionSubcommand, Name: "cancel", Description: "Cancel a planned power cut", Options: []appCommand{
				{
					Type:         optionString,
					Name:         "time",
					Description:  "The power cut to cancel",
					Required:     true,
					Autocomplete: true,
				},
			}},
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
