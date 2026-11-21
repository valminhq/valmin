-- An account whose password an administrator chose or issued is asked to replace it.
ALTER TABLE users ADD COLUMN password_change_required INTEGER NOT NULL DEFAULT 0;
