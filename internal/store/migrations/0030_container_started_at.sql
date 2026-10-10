-- When the observer last saw this instance's container start, so a start no job made is noticed once.
ALTER TABLE instances ADD COLUMN container_started_at TIMESTAMP;
