-- rule_hops.total_bytes held two units: the entry hop (position 0) carried the
-- rule's weighted traffic (direction-folded bytes x entry node multiplier x
-- owner billing rate), every other hop its own direction-folded bytes. The
-- per-hop tables of the rule and node detail pages showed both side by side
-- under one heading.
--
-- The rule's weighted traffic is an attribute of the rule and lives on its row.
-- On a hop row it could not survive: RegenerateRule deletes and re-inserts all
-- of a rule's hop rows, so every regeneration (an edit, a relay-address
-- rewire, a port reallocation) zeroed the rule's traffic while the owner's
-- usage kept it. rule_hops.total_bytes is direction-folded on every hop from
-- here on.
--
-- The entry hops' folded history cannot be recovered from a weighted total, so
-- their per-hop counter restarts at 0; the history itself carries over into
-- rules.weighted_bytes unchanged.
ALTER TABLE rules ADD COLUMN weighted_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE rules SET weighted_bytes = COALESCE(
    (SELECT SUM(rh.total_bytes) FROM rule_hops rh WHERE rh.rule_id = rules.id AND rh.position = 0),
    0);
UPDATE rule_hops SET total_bytes = 0 WHERE position = 0;
