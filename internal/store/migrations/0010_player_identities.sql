-- Accounts an instance's log named, one row per account per instance. The panel keeps these
-- so an operator can fill adminlist.txt, bannedlist.txt or permittedlist.txt from what the
-- server saw rather than by copying an id out of a console (03 §4).
--
-- platform_id is stored exactly as the server printed it, in the `Steam_<id>` form those
-- files accept; rewriting one form into the other could silently strip an admin of admin
-- (Q30). name is the display name last seen beside it, and is empty for the lines that carry
-- no name -- the id is the part an operator needs, and it is never withheld for want of one.
--
-- Seen, not played: the socket line that produces most of these rows is a connection attempt,
-- and one measured instance of it was rejected two seconds later for a wrong password.
CREATE TABLE player_identities (
    instance_id   TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    platform_id   TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (instance_id, platform_id)
);

CREATE INDEX idx_player_identities_seen
    ON player_identities (instance_id, last_seen_at DESC);
