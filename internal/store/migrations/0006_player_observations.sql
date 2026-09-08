-- Observed player counts over time, one row per change. The panel writes a row only when the
-- number it can report changes, so an idle server costs nothing and a busy one costs a handful
-- of rows an hour.
--
-- players is nullable and the null is the point: it means the panel stopped being able to say,
-- not that the server emptied. A log stream that restarted, a container that was replaced and a
-- peer that timed out without the server printing a new count all produce one. A reader that
-- renders null as zero turns "we were not looking" into "nobody was playing" (E7).
CREATE TABLE player_observations (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    observed_at TIMESTAMP NOT NULL,
    players     INTEGER
);

-- Serves both readers: the newest-first keyset page and the retention sweep.
CREATE INDEX idx_player_observations_instance
    ON player_observations(instance_id, observed_at DESC, id DESC);
