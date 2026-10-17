-- When the server last refused this account's connection for a wrong password.
ALTER TABLE player_identities ADD COLUMN wrong_password_at TIMESTAMP;
