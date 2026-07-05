-- Grant-level role mask overriding the node's roles for one user's use of the
-- node: 0 = inherit the node's mask, non-zero = use this mask instead (any
-- combination of entry=1 / via=2, independent of the node mask). Lets a
-- middle-layer node be opened as a rule entry for specific grantees without
-- changing its global role or exposing it to everyone else granted the node.
-- Existing grants inherit (0), so behavior is unchanged.
ALTER TABLE user_nodes ADD COLUMN roles INTEGER NOT NULL DEFAULT 0;
