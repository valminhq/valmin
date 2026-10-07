package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/discord"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/store"
)

// Discord serves the admin settings of the panel's Discord bot.
type Discord struct {
	DB     *store.DB
	Authz  *authz.Authz
	Keeper *crypto.Keeper
	Bot    *discord.Bot
}

func discordRoutes(rt *routeTable, h *Discord) {
	rt.Handle("GET /api/v1/admin/discord", http.HandlerFunc(h.get))
	rt.Handle("PUT /api/v1/admin/discord", http.HandlerFunc(h.put))
	rt.Handle("DELETE /api/v1/admin/discord", http.HandlerFunc(h.remove))
}

// snowflake is a Discord id.
var snowflake = regexp.MustCompile(`^\d{17,20}$`)

// discordView is the bot's settings and state. The token is never part of it.
type discordView struct {
	Configured bool                `json:"configured"`
	Enabled    bool                `json:"enabled"`
	Links      []store.DiscordLink `json:"links"`
	Status     discord.Status      `json:"status"`
	InviteURL  string              `json:"invite_url,omitempty"`
}

type discordLinkRequest struct {
	GuildID     string   `json:"guild_id"`
	ChannelID   string   `json:"channel_id"`
	AllowStart  bool     `json:"allow_start"`
	InstanceIDs []string `json:"instance_ids"`
}

type discordRequest struct {
	// Token replaces the stored token when set; absent or empty keeps it.
	Token   *string              `json:"token"`
	Enabled bool                 `json:"enabled"`
	Links   []discordLinkRequest `json:"links"`
}

func (h *Discord) get(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	h.writeView(r.Context(), w, r)
}

func (h *Discord) put(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body discordRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	current, err := h.DB.DiscordBot(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	newToken := body.Token != nil && *body.Token != ""
	links, ok := h.validate(w, r, &body, newToken || (current != nil && current.Token != ""))
	if !ok {
		return
	}

	bot := &store.DiscordBot{ID: store.NewID(), Enabled: body.Enabled}
	if current != nil {
		bot.ID, bot.Token = current.ID, current.Token
	}
	if newToken {
		bot.Token, err = h.Keeper.Encrypt(crypto.PurposeDiscordToken, crypto.DiscordTokenLocation(bot.ID),
			[]byte(*body.Token))
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
	}
	audit := &store.AuditEntry{
		UserID: u.ID, Action: authz.PanelSettings.String(), IP: middleware.ClientIPFrom(r.Context()).String(),
		Detail: fmt.Sprintf(`{"operation":"discord_update","enabled":%t,"links":%d,"token_changed":%t}`,
			body.Enabled, len(links), newToken),
	}
	if err := h.DB.SaveDiscordBot(r.Context(), bot, links, u.ID, audit); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.Bot.Reload()
	h.writeView(r.Context(), w, r)
}

// validate checks the links and returns them as rows. hasToken reports whether a token will be
// stored after this request.
func (h *Discord) validate(
	w http.ResponseWriter, r *http.Request, body *discordRequest, hasToken bool,
) ([]store.DiscordLink, bool) {
	var v apierr.Validation
	if body.Enabled && !hasToken {
		v.Add("token", apierr.FieldRequired, "Paste the bot token to turn the bot on.")
	}
	seen := map[string]bool{}
	links := make([]store.DiscordLink, 0, len(body.Links))
	for i, l := range body.Links {
		field := fmt.Sprintf("links[%d]", i)
		if !snowflake.MatchString(l.GuildID) {
			v.Add(field+".guild_id", apierr.FieldInvalid, "Paste the Discord server ID: a number of 17 to 20 digits.")
		}
		if l.ChannelID != "" && !snowflake.MatchString(l.ChannelID) {
			v.Add(field+".channel_id", apierr.FieldInvalid,
				"Paste the channel ID, a number of 17 to 20 digits, or leave it empty for every channel.")
		}
		if key := l.GuildID + "/" + l.ChannelID; seen[key] {
			v.Add(field, apierr.FieldInvalid, "This Discord server and channel are already linked above.")
		} else {
			seen[key] = true
		}
		ids := []string{}
		for _, id := range l.InstanceIDs {
			inst, err := h.DB.InstanceByID(r.Context(), id)
			if err != nil {
				apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
				return nil, false
			}
			if inst == nil {
				v.Add(field+".instance_ids", apierr.FieldNotAnOption, "One of the chosen servers no longer exists.")
				continue
			}
			ids = append(ids, id)
		}
		links = append(links, store.DiscordLink{
			GuildID: l.GuildID, ChannelID: l.ChannelID, AllowStart: l.AllowStart, InstanceIDs: ids,
		})
	}
	if err := v.Err(); err != nil {
		apierr.Write(w, r, err)
		return nil, false
	}
	return links, true
}

func (h *Discord) remove(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	audit := &store.AuditEntry{
		UserID: u.ID, Action: authz.PanelSettings.String(), IP: middleware.ClientIPFrom(r.Context()).String(),
		Detail: `{"operation":"discord_delete"}`,
	}
	if err := h.DB.DeleteDiscordBot(r.Context(), audit); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	h.Bot.Reload()
	w.WriteHeader(http.StatusNoContent)
}

// view is the stored settings and the bot's live state.
func (h *Discord) view(ctx context.Context) (*discordView, error) {
	bot, err := h.DB.DiscordBot(ctx)
	if err != nil {
		return nil, fmt.Errorf("discord settings: %w", err)
	}
	links, err := h.DB.DiscordLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("discord settings: %w", err)
	}
	view := &discordView{Links: links, Status: h.Bot.Status()}
	if bot != nil {
		view.Configured, view.Enabled = bot.Token != "", bot.Enabled
	}
	if id := view.Status.ApplicationID; id != "" {
		view.InviteURL = "https://discord.com/oauth2/authorize?client_id=" + url.QueryEscape(id) +
			"&scope=bot+applications.commands&permissions=0"
	}
	return view, nil
}

// writeView answers with view, or the error reading it.
func (h *Discord) writeView(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	view, err := h.view(ctx)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, view)
}
