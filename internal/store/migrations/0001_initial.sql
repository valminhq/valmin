-- Initial schema. 12 §10 settles Q18: at M1 there is no deployed schema, so 04 §2,
-- 07 §6's command-channel columns and 12's corrections all fold into one CREATE and
-- ALTER TABLE begins at the first tagged release.
--
-- Portable subset only (10 §4.3): TEXT ids (UUIDv7), UTC timestamps, no RETURNING, no
-- partial indexes, no dialect-specific JSON operators. Booleans are TRUE/FALSE rather
-- than 1/0 so the same DDL runs on Postgres.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    totp_secret   TEXT,
    disabled      BOOLEAN   NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMP NOT NULL,
    last_login_at TIMESTAMP
);

-- 10 §4.1. The cookie carries a 32-byte CSPRNG token; this stores SHA-256 of it. Not
-- argon2id: a high-entropy value looked up on every request wants a fast hash.
CREATE TABLE sessions (
    id                  TEXT PRIMARY KEY,
    token_hash          TEXT NOT NULL UNIQUE,
    user_id             TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at          TIMESTAMP NOT NULL,
    last_seen_at        TIMESTAMP NOT NULL,
    idle_expires_at     TIMESTAMP NOT NULL,
    absolute_expires_at TIMESTAMP NOT NULL,
    revoked_at          TIMESTAMP,
    ip                  TEXT,
    user_agent          TEXT
);

CREATE INDEX idx_sessions_user ON sessions (user_id);

-- 10 §4.2. Panel-owned scalars, deliberately not a settings system: no UI writes here.
-- Reserved keys: key_salt, active_key_id, argon2_params, bootstrap_state,
-- thunderstore_etag, thunderstore_synced_at, panel_id, daemon_lease, data_fs_type.
CREATE TABLE kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- 04 §2 with 12's corrections: autostart is deleted (ADR-033), and the state enum gains
-- provisioning and deleting (12 §2.1).
CREATE TABLE instances (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    state        TEXT NOT NULL CHECK (state IN (
        'created', 'provisioning', 'stopped', 'starting', 'running', 'stopping',
        'backing_up', 'restoring', 'updating', 'deleting', 'error'
    )),
    container_id TEXT,
    -- Host path, not the path inside the panel container (02 §5).
    data_dir     TEXT NOT NULL,
    base_port    INTEGER NOT NULL UNIQUE,

    server_name  TEXT NOT NULL,
    world_name   TEXT NOT NULL,
    -- Encrypted at rest (10 §3): a live game secret, not a credential to verify.
    password     TEXT NOT NULL,
    public       BOOLEAN NOT NULL DEFAULT FALSE,
    crossplay    BOOLEAN NOT NULL DEFAULT FALSE,
    -- -instanceid. Assigned at provision, immutable, set even when crossplay is off
    -- (ADR-027, 03 §1.4).
    crossplay_instance_id TEXT NOT NULL UNIQUE,
    preset       TEXT,
    modifiers    TEXT,
    -- Admin-only; shapes container creation (09 §3.3, 02 §6).
    extra_args   TEXT,

    modded           BOOLEAN NOT NULL DEFAULT FALSE,
    bepinex_version  TEXT,
    restart_required BOOLEAN NOT NULL DEFAULT FALSE,
    mem_limit_mb     INTEGER NOT NULL DEFAULT 4096,
    cpu_limit        REAL,
    game_build_id    TEXT,

    -- 07 §6, folded in rather than ALTERed (Q18, 12 §10).
    command_channel    TEXT NOT NULL DEFAULT 'auto'
        CHECK (command_channel IN ('auto', 'stdin', 'rcon', 'none')),
    stdin_probe_result BOOLEAN,
    stdin_probe_build  TEXT,
    rcon_port          INTEGER,
    -- Encrypted at rest (10 §3).
    rcon_password      TEXT,

    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- 09 §3. perms is additive only; effective access is roleDefaults[role] union perms.
CREATE TABLE instance_grants (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'operator')),
    perms       TEXT NOT NULL DEFAULT '[]',
    granted_by  TEXT REFERENCES users (id),
    granted_at  TIMESTAMP NOT NULL,
    -- Enforced by Can() from M1; only the UI for setting it is deferred (09 §4, Q10).
    expires_at  TIMESTAMP,
    PRIMARY KEY (user_id, instance_id)
);

CREATE INDEX idx_instance_grants_instance ON instance_grants (instance_id);

-- 09 §5. An invite is a credential: argon2id-hashed, shown once, never stored plain.
CREATE TABLE invites (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    created_by  TEXT NOT NULL REFERENCES users (id),
    instance_id TEXT REFERENCES instances (id) ON DELETE CASCADE,
    grant_role  TEXT CHECK (grant_role IS NULL OR grant_role IN ('viewer', 'operator')),
    grant_perms TEXT NOT NULL DEFAULT '[]',
    expires_at  TIMESTAMP NOT NULL,
    redeemed_at TIMESTAMP,
    redeemed_by TEXT REFERENCES users (id),
    revoked_at  TIMESTAMP,
    created_at  TIMESTAMP NOT NULL
);

CREATE TABLE mod_packages (
    full_name      TEXT PRIMARY KEY,
    namespace      TEXT NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT,
    latest_version TEXT,
    downloads      INTEGER,
    rating         INTEGER,
    is_deprecated  BOOLEAN NOT NULL DEFAULT FALSE,
    categories     TEXT,
    icon_url       TEXT,
    synced_at      TIMESTAMP NOT NULL
);

CREATE TABLE mod_versions (
    full_name    TEXT NOT NULL,
    version      TEXT NOT NULL,
    dependencies TEXT NOT NULL,
    download_url TEXT NOT NULL,
    file_size    INTEGER,
    PRIMARY KEY (full_name, version)
);

-- file_manifest is what makes uninstall exact (ADR-009, 03 §6.4). Retrofitting it would
-- leave every already-installed mod unremovable.
CREATE TABLE instance_mods (
    instance_id   TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    full_name     TEXT NOT NULL,
    version       TEXT NOT NULL,
    installed_as  TEXT NOT NULL CHECK (installed_as IN ('explicit', 'dependency')),
    side          TEXT NOT NULL DEFAULT 'unknown' CHECK (side IN (
        'server_only', 'client_required', 'client_optional', 'unknown'
    )),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    file_manifest TEXT NOT NULL,
    installed_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (instance_id, full_name)
);

-- consistent is FALSE for hot copies, which skip the quiesce (12 §2.3, 02 §4.4).
-- pre_import is the snapshot world_import takes before replacing a world (03 §4.1).
CREATE TABLE backups (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL,
    sha256      TEXT NOT NULL,
    world_name  TEXT NOT NULL,
    trigger     TEXT NOT NULL CHECK (trigger IN (
        'manual', 'scheduled', 'pre_update', 'pre_restore', 'pre_import'
    )),
    consistent  BOOLEAN NOT NULL,
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX idx_backups_instance ON backups (instance_id, created_at);

CREATE TABLE scheduled_jobs (
    id          TEXT PRIMARY KEY,
    instance_id TEXT REFERENCES instances (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    cron        TEXT NOT NULL,
    payload     TEXT,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    last_run_at TIMESTAMP,
    next_run_at TIMESTAMP
);

-- 12 §4.1. instance_id is ON DELETE SET NULL, not cascade (12 §4.2): the delete job is
-- itself a row carrying that instance's id, so a cascade would delete the job at the
-- moment it succeeds. instance_name is denormalised so the history stays readable.
CREATE TABLE job_runs (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN (
        'queued', 'running', 'succeeded', 'failed', 'cancelled'
    )),
    lock_key            TEXT NOT NULL,
    instance_id         TEXT REFERENCES instances (id) ON DELETE SET NULL,
    instance_name       TEXT,
    schedule_id         TEXT REFERENCES scheduled_jobs (id) ON DELETE SET NULL,
    payload             TEXT NOT NULL DEFAULT '{}',
    checkpoint          TEXT,
    -- Resume intent, honoured only for kinds that cannot leave world data
    -- half-written (ADR-032, 12 §9.3).
    resume_after        BOOLEAN NOT NULL DEFAULT FALSE,
    progress            INTEGER NOT NULL DEFAULT 0,
    message             TEXT,
    lease_owner         TEXT,
    lease_until         TIMESTAMP,
    cancel_requested_at TIMESTAMP,
    requested_by        TEXT REFERENCES users (id) ON DELETE SET NULL,
    attempt             INTEGER NOT NULL DEFAULT 1,
    error_code          TEXT,
    error               TEXT,
    log                 TEXT,
    created_at          TIMESTAMP NOT NULL,
    started_at          TIMESTAMP,
    finished_at         TIMESTAMP
);

CREATE INDEX idx_job_runs_instance ON job_runs (instance_id, created_at);
CREATE INDEX idx_job_runs_status ON job_runs (status, lease_until);

-- 12 §4.3. Acquire is an INSERT; a primary-key conflict is the "already running" answer.
-- Not a partial unique index on job_runs, which is outside the portable subset.
CREATE TABLE job_locks (
    lock_key    TEXT PRIMARY KEY,
    job_id      TEXT NOT NULL,
    acquired_at TIMESTAMP NOT NULL
);

-- The permanent record of who did what. Never cascades: deleting an instance must not
-- erase the audit trail of what was done to it (12 §4.2, 09 §4).
CREATE TABLE audit_log (
    id          TEXT PRIMARY KEY,
    user_id     TEXT,
    instance_id TEXT,
    action      TEXT NOT NULL,
    detail      TEXT,
    ip          TEXT,
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX idx_audit_log_created ON audit_log (created_at);
CREATE INDEX idx_audit_log_instance ON audit_log (instance_id, created_at);
