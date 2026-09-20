-- An operational condition believed true right now, opened by the scan that first observes it
-- and resolved by the scan that no longer does. The two timestamps are what let the inbox say
-- how long something has been wrong and let a rule fire once per edge rather than per scan.
--
-- instance_id is null for a condition belonging to the host: free space is a property of the
-- shared data root, so per-instance rows would repeat one number. detail carries the kind's own
-- fields as JSON and nothing branches on it.
CREATE TABLE alert_conditions (
    id            TEXT PRIMARY KEY,
    instance_id   TEXT REFERENCES instances (id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '{}',
    first_seen_at TIMESTAMP NOT NULL,
    last_seen_at  TIMESTAMP NOT NULL,
    resolved_at   TIMESTAMP
);

-- One open row per kind per instance, while still admitting resolved history. Both nullable
-- columns are folded through COALESCE because SQLite treats nulls as distinct in a unique
-- index, which would let the host row reopen on every scan. Keying on resolved_at is safe
-- because a stored timestamp carries a fixed nine-digit fraction.
CREATE UNIQUE INDEX idx_alert_conditions_unique ON alert_conditions (
    kind, COALESCE(instance_id, ''), COALESCE(resolved_at, '')
);

CREATE INDEX idx_alert_conditions_open ON alert_conditions (resolved_at, first_seen_at DESC);

-- A rule routing one condition kind to a set of destinations. instance_id is null for a rule
-- covering every instance, including servers created after it was written.
--
-- params holds the kind's thresholds as JSON; a missing field reads as its default. quiet_start
-- and quiet_end are minutes from local midnight in quiet_tz, all three null together when the
-- rule has no quiet hours, and a window past midnight has a start above its end.
CREATE TABLE alert_rules (
    id             TEXT PRIMARY KEY,
    instance_id    TEXT REFERENCES instances (id) ON DELETE CASCADE,
    condition_kind TEXT NOT NULL,
    params         TEXT NOT NULL DEFAULT '{}',
    quiet_start    INTEGER CHECK (quiet_start BETWEEN 0 AND 1439),
    quiet_end      INTEGER CHECK (quiet_end BETWEEN 0 AND 1439),
    quiet_tz       TEXT,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_by     TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL,
    CHECK ((quiet_start IS NULL) = (quiet_end IS NULL)),
    CHECK ((quiet_start IS NULL) = (quiet_tz IS NULL))
);

CREATE INDEX idx_alert_rules_kind ON alert_rules (condition_kind);

-- Which destinations a rule sends to. A join table rather than a list of ids on the rule, so
-- deleting a destination removes it from every rule that named it.
CREATE TABLE alert_rule_destinations (
    rule_id    TEXT NOT NULL REFERENCES alert_rules (id) ON DELETE CASCADE,
    webhook_id TEXT NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    PRIMARY KEY (rule_id, webhook_id)
);

-- One row per edge per rule per condition. The primary key is the deduplication: dispatch
-- inserts before it sends and treats a conflict as already told.
CREATE TABLE alert_notifications (
    condition_id TEXT NOT NULL REFERENCES alert_conditions (id) ON DELETE CASCADE,
    rule_id      TEXT NOT NULL REFERENCES alert_rules (id) ON DELETE CASCADE,
    edge         TEXT NOT NULL CHECK (edge IN ('opened', 'resolved')),
    notified_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (condition_id, rule_id, edge)
);

-- A server going down on its own, recorded where the observer detects it (C14). Kept as its own
-- fact because a crash loop is a rate and cannot be recovered from state: an instance that
-- crashes and restarts within a scan interval is never observed down. Swept with the other
-- retention at daemon start.
CREATE TABLE instance_incidents (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    reason      TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_instance_incidents_recent
    ON instance_incidents (instance_id, occurred_at DESC);
