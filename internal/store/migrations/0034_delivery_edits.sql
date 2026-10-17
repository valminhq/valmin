-- remote_id is the receiver's id for the message a delivery created, so a later delivery can edit
-- that message. edits_id names the delivery whose message this one edits in place: a resolved
-- alert edits the alert it resolves.
ALTER TABLE webhook_deliveries ADD COLUMN remote_id TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN edits_id TEXT REFERENCES webhook_deliveries (id) ON DELETE SET NULL;

-- The event an edge was sent as, so a resolution can find the deliveries of its opening.
ALTER TABLE alert_notifications ADD COLUMN event_id TEXT;
