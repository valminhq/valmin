-- The IANA zone a user reads times in and creates schedules in. '' follows their browser.
ALTER TABLE users ADD COLUMN timezone TEXT NOT NULL DEFAULT '';
