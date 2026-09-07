-- The owner is the bootstrap account (09 §2): an admin the panel refuses to demote, disable
-- or delete, so no sequence of user administration can leave a panel with zero admins.
-- A column rather than a third `role` value: widening role's CHECK means rebuilding `users`,
-- and a migration runs inside a transaction with foreign keys on, where dropping a parent
-- table cascades every session, grant and invite away.

ALTER TABLE users ADD COLUMN owner BOOLEAN NOT NULL DEFAULT FALSE;

-- An existing panel adopts its first admin. A fresh one has no rows here; CreateFirstAdmin
-- sets the flag at bootstrap.
UPDATE users SET owner = TRUE
WHERE id = (SELECT id FROM users WHERE role = 'admin' ORDER BY created_at, id LIMIT 1);

CREATE UNIQUE INDEX idx_users_owner ON users (owner) WHERE owner;
