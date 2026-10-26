-- When an update-test copy's test ran to the end. An apply is refused for a copy without it,
-- since a failed or cancelled test can leave the copy with only part of the update.
ALTER TABLE instances ADD COLUMN test_passed_at TIMESTAMP;
