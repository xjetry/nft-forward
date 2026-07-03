-- roles is a bitmask: bit0 = entry (rule-selectable entry node),
-- bit1 = via (middle-layer segment attachable behind an upstream node).
-- Every pre-existing node is an entry so current behavior is unchanged.
ALTER TABLE nodes ADD COLUMN roles INTEGER NOT NULL DEFAULT 1;
