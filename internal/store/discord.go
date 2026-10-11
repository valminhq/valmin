package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DiscordBot is the panel's one Discord bot. Token is the encrypted envelope, empty when none
// is stored. AdminIDs are the Discord user or role ids whose holders may run the bot's admin
// commands, and Timezone is the IANA zone the times they type are read in.
type DiscordBot struct {
	ID        string
	Token     string
	Enabled   bool
	AdminIDs  []string
	Timezone  string
	UpdatedAt time.Time
}

// DiscordLink ties a Discord server, or one channel in it, to the panel servers its commands
// may reach. An empty ChannelID covers every channel of the Discord server.
type DiscordLink struct {
	ID          string   `json:"id"`
	GuildID     string   `json:"guild_id"`
	ChannelID   string   `json:"channel_id"`
	AllowStart  bool     `json:"allow_start"`
	InstanceIDs []string `json:"instance_ids"`
}

// DiscordBot returns the stored bot, or nil when none is configured.
func (db *DB) DiscordBot(ctx context.Context) (*DiscordBot, error) {
	var b DiscordBot
	var adminIDs, updatedAt string
	err := db.Reader.QueryRowContext(ctx,
		`SELECT id, token, enabled, admin_ids, timezone, updated_at FROM discord_bot`).
		Scan(&b.ID, &b.Token, &b.Enabled, &adminIDs, &b.Timezone, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read discord bot: %w", err)
	}
	if err := json.Unmarshal([]byte(adminIDs), &b.AdminIDs); err != nil {
		return nil, fmt.Errorf("discord bot admin_ids: %w", err)
	}
	if b.UpdatedAt, err = ParseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("discord bot updated_at: %w", err)
	}
	return &b, nil
}

// DiscordLinks returns every link with its servers, ordered by Discord server and channel.
func (db *DB) DiscordLinks(ctx context.Context) ([]DiscordLink, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT l.id, l.guild_id, l.channel_id, l.allow_start, COALESCE(i.instance_id, '')
		FROM discord_links l LEFT JOIN discord_link_instances i ON i.link_id = l.id
		ORDER BY l.guild_id, l.channel_id, i.instance_id`)
	if err != nil {
		return nil, fmt.Errorf("list discord links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	links := []DiscordLink{}
	for rows.Next() {
		var l DiscordLink
		var instanceID string
		if err := rows.Scan(&l.ID, &l.GuildID, &l.ChannelID, &l.AllowStart, &instanceID); err != nil {
			return nil, fmt.Errorf("scan discord link: %w", err)
		}
		if n := len(links); n == 0 || links[n-1].ID != l.ID {
			l.InstanceIDs = []string{}
			links = append(links, l)
		}
		if instanceID != "" {
			last := &links[len(links)-1]
			last.InstanceIDs = append(last.InstanceIDs, instanceID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list discord links: %w", err)
	}
	return links, nil
}

// DiscordLinkFor returns the link that governs a channel: one naming the channel itself, then
// one naming its parent (for a thread), then one covering the whole Discord server. It returns
// nil when none does.
func (db *DB) DiscordLinkFor(ctx context.Context, guildID, channelID, parentID string) (*DiscordLink, error) {
	links, err := db.DiscordLinks(ctx)
	if err != nil {
		return nil, err
	}
	for n, channel := range []string{channelID, parentID, ""} {
		if channel == "" && n < 2 {
			continue
		}
		for i := range links {
			if links[i].GuildID == guildID && links[i].ChannelID == channel {
				return &links[i], nil
			}
		}
	}
	return nil, nil
}

// DiscordGuilds returns every Discord server some link names.
func (db *DB) DiscordGuilds(ctx context.Context) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT DISTINCT guild_id FROM discord_links ORDER BY guild_id`)
	if err != nil {
		return nil, fmt.Errorf("list discord guilds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var guilds []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan discord guild: %w", err)
		}
		guilds = append(guilds, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list discord guilds: %w", err)
	}
	return guilds, nil
}

// SaveDiscordBot stores the bot and replaces every link with links, together with audit, in one
// transaction. bot.ID names the existing row when there is one.
func (db *DB) SaveDiscordBot(
	ctx context.Context, bot *DiscordBot, links []DiscordLink, updatedBy string, audit *AuditEntry,
) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin discord bot update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	adminIDs, err := json.Marshal(append([]string{}, bot.AdminIDs...))
	if err != nil {
		return fmt.Errorf("encode discord bot admin ids: %w", err)
	}
	zone := bot.Timezone
	if zone == "" {
		zone = "UTC"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO discord_bot (id, token, enabled, admin_ids, timezone, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?)
		ON CONFLICT (id) DO UPDATE SET token = excluded.token, enabled = excluded.enabled,
			admin_ids = excluded.admin_ids, timezone = excluded.timezone,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		bot.ID, bot.Token, bot.Enabled, string(adminIDs), zone, updatedBy, FormatTime(now)); err != nil {
		return fmt.Errorf("save discord bot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM discord_links`); err != nil {
		return fmt.Errorf("clear discord links: %w", err)
	}
	for i := range links {
		l := &links[i]
		if l.ID == "" {
			l.ID = NewID()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO discord_links (id, guild_id, channel_id, allow_start) VALUES (?, ?, ?, ?)`,
			l.ID, l.GuildID, l.ChannelID, l.AllowStart); err != nil {
			return fmt.Errorf("save discord link: %w", err)
		}
		for _, instanceID := range l.InstanceIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO discord_link_instances (link_id, instance_id) VALUES (?, ?)`,
				l.ID, instanceID); err != nil {
				return fmt.Errorf("save discord link server: %w", err)
			}
		}
	}
	if audit != nil {
		if err := writeAuditLog(ctx, tx, audit, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit discord bot update: %w", err)
	}
	return nil
}

// DeleteDiscordBot removes the bot and every link, together with audit.
func (db *DB) DeleteDiscordBot(ctx context.Context, audit *AuditEntry) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin discord bot delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM discord_links`, `DELETE FROM discord_bot`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("delete discord bot: %w", err)
		}
	}
	if audit != nil {
		if err := writeAuditLog(ctx, tx, audit, time.Now().UTC()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit discord bot delete: %w", err)
	}
	return nil
}

// DisableDiscordBot turns the bot off without touching its links.
func (db *DB) DisableDiscordBot(ctx context.Context) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE discord_bot SET enabled = FALSE, updated_at = ?`, Now()); err != nil {
		return fmt.Errorf("disable discord bot: %w", err)
	}
	return nil
}
