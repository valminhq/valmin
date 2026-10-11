package store

import (
	"slices"
	"testing"
)

// TestDiscordLinkForPrefersTheNarrowestLink asserts a channel's own link wins over its parent's,
// and the parent's over the whole Discord server's.
func TestDiscordLinkForPrefersTheNarrowestLink(t *testing.T) {
	db := open(t)
	seedInstance(t, db, "a", 2456)
	links := []DiscordLink{
		{GuildID: "g1", ChannelID: "", InstanceIDs: []string{"a"}},
		{GuildID: "g1", ChannelID: "c1", AllowStart: true, InstanceIDs: []string{}},
		{GuildID: "g1", ChannelID: "parent", InstanceIDs: []string{}},
	}
	if err := db.SaveDiscordBot(t.Context(), &DiscordBot{ID: "bot", Token: "env"}, links, "", nil); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, guild, channel, parent, want string
	}{
		{"the channel's own link", "g1", "c1", "parent", "c1"},
		{"a thread's parent", "g1", "thread", "parent", "parent"},
		{"the whole Discord server", "g1", "other", "", ""},
		{"another Discord server", "g2", "c1", "", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.DiscordLinkFor(t.Context(), tt.guild, tt.channel, tt.parent)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.want == "none" && got != nil:
				t.Fatalf("link = %+v, want none", got)
			case tt.want != "none" && (got == nil || got.ChannelID != tt.want):
				t.Fatalf("link = %+v, want channel %q", got, tt.want)
			}
		})
	}
}

// TestSaveDiscordBotReplacesEveryLink asserts a save replaces the whole set, keeps the bot row
// single, and that deleting a server drops it from the links.
func TestSaveDiscordBotReplacesEveryLink(t *testing.T) {
	db := open(t)
	seedInstance(t, db, "a", 2456)
	seedInstance(t, db, "b", 2466)
	bot := &DiscordBot{ID: "bot", Token: "env", Enabled: true}
	first := []DiscordLink{
		{GuildID: "g1", InstanceIDs: []string{"a", "b"}},
		{GuildID: "g2", InstanceIDs: []string{"a"}},
	}
	if err := db.SaveDiscordBot(t.Context(), bot, first, "", nil); err != nil {
		t.Fatal(err)
	}
	bot.Enabled, bot.AdminIDs, bot.Timezone = false, []string{"111", "222"}, "Europe/Kyiv"
	second := []DiscordLink{{GuildID: "g3", ChannelID: "c", AllowStart: true, InstanceIDs: []string{"a", "b"}}}
	if err := db.SaveDiscordBot(t.Context(), bot, second, "", nil); err != nil {
		t.Fatal(err)
	}
	stored, err := db.DiscordBot(t.Context())
	if err != nil || stored == nil || stored.Enabled || stored.Token != "env" ||
		!slices.Equal(stored.AdminIDs, []string{"111", "222"}) || stored.Timezone != "Europe/Kyiv" {
		t.Fatalf("bot = %+v, %v", stored, err)
	}
	links, err := db.DiscordLinks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].GuildID != "g3" || !slices.Equal(links[0].InstanceIDs, []string{"a", "b"}) {
		t.Fatalf("links = %+v", links)
	}

	exec(t, db.Writer, `DELETE FROM instances WHERE id = 'b'`)
	links, err = db.DiscordLinks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(links[0].InstanceIDs, []string{"a"}) {
		t.Fatalf("servers after delete = %v, want [a]", links[0].InstanceIDs)
	}

	guilds, err := db.DiscordGuilds(t.Context())
	if err != nil || !slices.Equal(guilds, []string{"g3"}) {
		t.Fatalf("guilds = %v, %v", guilds, err)
	}
}

// TestTheDiscordTokenIsASweptSecret asserts key rotation and key-loss recovery see the token.
func TestTheDiscordTokenIsASweptSecret(t *testing.T) {
	db := open(t)
	if err := db.SaveDiscordBot(t.Context(), &DiscordBot{ID: "bot", Token: "v1.k.n.ct"}, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	secrets, err := db.ListSecrets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(secrets, func(s StaleSecret) bool {
		return s.Table == "discord_bot" && s.Column == "token" && s.RowID == "bot" && s.Purpose == "discord-token"
	}) {
		t.Fatalf("secrets = %+v, want the discord token", secrets)
	}
}
