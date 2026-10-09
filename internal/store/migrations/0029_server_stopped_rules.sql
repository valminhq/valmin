-- An auto-stop is now one reason a server_stopped message names, not a rule kind of its own.
UPDATE alert_rules SET condition_kind = 'server_stopped' WHERE condition_kind = 'auto_stopped';
