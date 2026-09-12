-- A definition operation is the create/import chain's persisted intent (Q52). ADR-116 runs
-- the chain as separate jobs, so the record of what the chain still owes has to outlive any
-- one of them.
--
-- steps is an ordered JSON array of {kind, ref, job_id}; cursor is the index of the next
-- outstanding step and advances inside its job's finish transaction. plan carries the
-- requested configuration the remaining steps need — mods, config bytes and the start
-- intent. Neither column ever holds a password or a browser-local file reference.
CREATE TABLE instance_operations (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('create', 'import')),
    state       TEXT NOT NULL CHECK (state IN (
        'running', 'interrupted', 'completed', 'abandoned'
    )),
    steps       TEXT NOT NULL,
    cursor      INTEGER NOT NULL DEFAULT 0,
    plan        TEXT NOT NULL DEFAULT '{}',
    created_by  TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);

-- One outstanding operation per instance. A finished operation keeps its row for history.
CREATE UNIQUE INDEX instance_operations_open
    ON instance_operations (instance_id)
    WHERE state IN ('running', 'interrupted');
