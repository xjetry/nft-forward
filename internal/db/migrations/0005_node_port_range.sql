-- Add per-node configurable port range (composite format like "10001-19999,23333,40000-42000").
-- Existing nodes default to the standard 10001-20000 range.
ALTER TABLE nodes ADD COLUMN port_range TEXT NOT NULL DEFAULT '10001-20000';
