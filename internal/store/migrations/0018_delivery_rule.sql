-- The alert rule whose dispatch wrote the delivery, so a rule's deliveries can be listed. Null
-- when no rule sent the event, and on rows written before this column existed.
ALTER TABLE webhook_deliveries ADD COLUMN rule_id TEXT REFERENCES alert_rules (id) ON DELETE SET NULL;

CREATE INDEX webhook_deliveries_rule ON webhook_deliveries (rule_id, created_at DESC, id DESC);
