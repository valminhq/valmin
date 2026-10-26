-- Every name an account has been seen with on one server. exact is set when the name was read
-- on the same line as the platform id; hits counts the spawns that happened while the account
-- was the only candidate or one of several.
CREATE TABLE player_names (
    instance_id   TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    platform_id   TEXT NOT NULL,
    name          TEXT NOT NULL,
    exact         INTEGER NOT NULL DEFAULT 0,
    hits          INTEGER NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (instance_id, platform_id, name)
);

INSERT INTO player_names (instance_id, platform_id, name, exact, hits, first_seen_at, last_seen_at)
SELECT instance_id, platform_id, name, 1, 0, first_seen_at, last_seen_at
FROM player_identities WHERE name <> '';
