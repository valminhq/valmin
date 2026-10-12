-- The Steam branch the server installs from, and the encrypted password of a password branch.
ALTER TABLE instances ADD COLUMN game_branch TEXT NOT NULL DEFAULT 'public';
ALTER TABLE instances ADD COLUMN game_branch_password TEXT;
