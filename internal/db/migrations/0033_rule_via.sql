-- via_node_ids: ordered JSON array of the middle-layer node ids a rule's
-- chain runs through, persisted on the rule so every re-derivation path keeps
-- the layers instead of silently dropping them.
ALTER TABLE rules ADD COLUMN via_node_ids TEXT NOT NULL DEFAULT '[]';
-- via_node_id: which logical segment a physical hop was expanded from
-- (entry segment = rules.node_id). Quota suppression, per-grant accounting
-- and shaping group by it.
ALTER TABLE rule_hops ADD COLUMN via_node_id INTEGER NOT NULL DEFAULT 0;
UPDATE rule_hops SET via_node_id = (SELECT node_id FROM rules WHERE rules.id = rule_hops.rule_id);
