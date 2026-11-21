-- The dispatcher polls pending deliveries oldest first, and resolving an alert finds the
-- delivery that announced its opening by event and destination. Both were walks of the whole
-- delivery history.
CREATE INDEX webhook_deliveries_pending ON webhook_deliveries (status, created_at, id);
CREATE INDEX webhook_deliveries_event ON webhook_deliveries (event_id, webhook_id);

-- Deleting a delivery sets edits_id to null on the deliveries that edit it; without this index
-- each deleted row would scan the whole table for them.
CREATE INDEX webhook_deliveries_edits ON webhook_deliveries (edits_id);
