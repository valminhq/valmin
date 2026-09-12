-- A webhook destination. url holds the AEAD envelope of 10 §3.2, never the plain URL: a
-- Discord webhook URL is the whole authentication for posting to that channel, so it is a
-- bearer credential and is stored the way every other secret is (10 §3).
CREATE TABLE webhooks (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('discord', 'generic')),
    url        TEXT NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX webhooks_name ON webhooks (name);

-- One attempt record per destination per event. The row is written with the change that
-- caused it and outlives the delivery job, so an exhausted delivery stays inspectable
-- instead of surviving only as a log line. payload is the rendered event value; it carries
-- no URL, no credential and no path.
CREATE TABLE webhook_deliveries (
    id          TEXT PRIMARY KEY,
    webhook_id  TEXT NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id    TEXT NOT NULL,
    event_kind  TEXT NOT NULL,
    instance_id TEXT REFERENCES instances (id) ON DELETE SET NULL,
    payload     TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);

CREATE INDEX webhook_deliveries_recent ON webhook_deliveries (created_at DESC, id DESC);
