-- A planned power cut. Every running server is stopped shortly before power_off_at.
-- created_by_name names whoever planned it when that was not a panel account.
CREATE TABLE planned_shutdowns (
    id              TEXT PRIMARY KEY,
    power_off_at    TIMESTAMP NOT NULL,
    created_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_by_name TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL
);
CREATE INDEX idx_planned_shutdowns_at ON planned_shutdowns (power_off_at);

-- Discord user or role ids whose holders may run the bot's admin commands, as a JSON array,
-- and the IANA zone the times they type are read in.
ALTER TABLE discord_bot ADD COLUMN admin_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE discord_bot ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
