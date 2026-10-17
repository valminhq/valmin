-- How many of its own rolling backups the game keeps (-backups).
ALTER TABLE instances ADD COLUMN game_backups INTEGER NOT NULL DEFAULT 4;
