-- How a user reads times and dates. '' follows their browser.
ALTER TABLE users ADD COLUMN hour_cycle TEXT NOT NULL DEFAULT '' CHECK (hour_cycle IN ('', 'h12', 'h23'));
ALTER TABLE users ADD COLUMN date_order TEXT NOT NULL DEFAULT '' CHECK (date_order IN ('', 'mdy', 'dmy', 'ymd'));
