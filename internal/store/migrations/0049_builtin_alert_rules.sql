-- Each channel that posted the built-in alerts is named instead by an ordinary rule of its own
-- scope for each of them: one rule per kind and scope. A new game build only ever reached shared
-- channels, and a channel a rule of the kind already names keeps only that rule.
INSERT INTO alert_rules (id, instance_id, condition_kind, created_at, updated_at)
SELECT substr(MIN(w.id), 1, 24) || '00000000000' || k.suffix, w.instance_id, k.kind,
       MIN(w.created_at), MIN(w.created_at)
FROM webhooks w
JOIN (
    SELECT 'instance_down' AS kind, '1' AS suffix
    UNION ALL SELECT 'backup_failed', '2'
    UNION ALL SELECT 'new_game_build', '3'
) k ON k.kind <> 'new_game_build' OR w.instance_id IS NULL
WHERE w.catch_all
  AND NOT EXISTS (
    SELECT 1 FROM alert_rules r
    JOIN alert_rule_destinations d ON d.rule_id = r.id
    WHERE d.webhook_id = w.id AND r.enabled AND r.condition_kind = k.kind
      AND COALESCE(r.instance_id, '') = COALESCE(w.instance_id, '')
  )
GROUP BY w.instance_id, k.kind, k.suffix;

-- The rules above have no destinations yet; their ids end in a suffix fixed per kind.
INSERT INTO alert_rule_destinations (rule_id, webhook_id)
SELECT r.id, w.id
FROM alert_rules r
JOIN webhooks w ON COALESCE(w.instance_id, '') = COALESCE(r.instance_id, '')
WHERE w.catch_all
  AND substr(r.id, 25) = '00000000000' || CASE r.condition_kind
      WHEN 'instance_down' THEN '1'
      WHEN 'backup_failed' THEN '2'
      WHEN 'new_game_build' THEN '3'
  END
  AND NOT EXISTS (SELECT 1 FROM alert_rule_destinations d0 WHERE d0.rule_id = r.id)
  AND NOT EXISTS (
    SELECT 1 FROM alert_rules o
    JOIN alert_rule_destinations d ON d.rule_id = o.id
    WHERE d.webhook_id = w.id AND o.id <> r.id AND o.enabled
      AND o.condition_kind = r.condition_kind
      AND COALESCE(o.instance_id, '') = COALESCE(r.instance_id, '')
  );

ALTER TABLE webhooks DROP COLUMN catch_all;
