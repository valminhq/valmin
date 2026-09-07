-- User deletion removes credentials issued by that user while preserving historical rows that
-- only name them. Grants owned by the deleted user still cascade through user_id.

CREATE TABLE instance_grants_new (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'operator')),
    perms       TEXT NOT NULL DEFAULT '[]',
    granted_by  TEXT REFERENCES users (id) ON DELETE SET NULL,
    granted_at  TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP,
    PRIMARY KEY (user_id, instance_id)
);

INSERT INTO instance_grants_new (
    user_id, instance_id, role, perms, granted_by, granted_at, expires_at
)
SELECT user_id, instance_id, role, perms, granted_by, granted_at, expires_at
FROM instance_grants;

DROP TABLE instance_grants;
ALTER TABLE instance_grants_new RENAME TO instance_grants;
CREATE INDEX idx_instance_grants_instance ON instance_grants (instance_id);

CREATE TABLE invites_new (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    created_by  TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    instance_id TEXT REFERENCES instances (id) ON DELETE CASCADE,
    grant_role  TEXT CHECK (grant_role IS NULL OR grant_role IN ('viewer', 'operator')),
    grant_perms TEXT NOT NULL DEFAULT '[]',
    expires_at  TIMESTAMP NOT NULL,
    redeemed_at TIMESTAMP,
    redeemed_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    revoked_at  TIMESTAMP,
    created_at  TIMESTAMP NOT NULL
);

INSERT INTO invites_new (
    id, token_hash, created_by, instance_id, grant_role, grant_perms,
    expires_at, redeemed_at, redeemed_by, revoked_at, created_at
)
SELECT id, token_hash, created_by, instance_id, grant_role, grant_perms,
       expires_at, redeemed_at, redeemed_by, revoked_at, created_at
FROM invites;

DROP TABLE invites;
ALTER TABLE invites_new RENAME TO invites;
