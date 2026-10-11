-- The panel's one Discord bot. token is an encrypted envelope; '' means none is stored.
CREATE TABLE discord_bot (
    id         TEXT PRIMARY KEY,
    token      TEXT NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX idx_discord_bot_singleton ON discord_bot ((1));

-- A Discord server, or one channel in it, and the panel servers its commands may reach.
-- channel_id '' covers every channel of the Discord server.
CREATE TABLE discord_links (
    id          TEXT PRIMARY KEY,
    guild_id    TEXT NOT NULL,
    channel_id  TEXT NOT NULL DEFAULT '',
    allow_start BOOLEAN NOT NULL DEFAULT TRUE,
    UNIQUE (guild_id, channel_id)
);

CREATE TABLE discord_link_instances (
    link_id     TEXT NOT NULL REFERENCES discord_links(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    PRIMARY KEY (link_id, instance_id)
);
