-- via_node_ids: ordered JSON array of the middle-layer node ids a rule's
-- chain runs through, persisted on the rule so every re-derivation path keeps
-- the layers instead of silently dropping them.
ALTER TABLE rules ADD COLUMN via_node_ids TEXT NOT NULL DEFAULT '[]';
-- via_node_id: which logical segment a physical hop was expanded from
-- (entry segment = rules.node_id). Quota suppression, per-grant accounting
-- and shaping group by it.
ALTER TABLE rule_hops ADD COLUMN via_node_id INTEGER NOT NULL DEFAULT 0;
UPDATE rule_hops SET via_node_id = (SELECT node_id FROM rules WHERE rules.id = rule_hops.rule_id);
-- Invariant: via_node_id names the logical segment a hop belongs to. A
-- composite-entry chain is one entry segment, so every hop keeps the entry
-- (composite) node id and the whole chain bills and suppresses on the composite
-- grant. An explicit-hops chain (non-composite entry) is instead one segment per
-- physical hop, so each hop's segment is itself: rewrite every hop to its own
-- node_id. Without this, downstream hops would carry the entry id and their
-- per-node grants would stop metering and quota-suppressing. Single-hop rules
-- are unaffected: the lone hop's node_id already equals rules.node_id.
UPDATE rule_hops SET via_node_id = node_id
WHERE rule_id IN (SELECT r.id FROM rules r JOIN nodes n ON n.id = r.node_id WHERE n.node_type != 'composite');
