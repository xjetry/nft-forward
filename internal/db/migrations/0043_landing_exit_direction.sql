-- Directional split of the exit ledger. used_bytes stays the authoritative
-- total; the split columns accumulate from this migration on, so for
-- pre-existing rows up+down <= used_bytes and the gap is history that
-- cannot be attributed to a direction.
ALTER TABLE user_landing_exits ADD COLUMN used_bytes_up INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_landing_exits ADD COLUMN used_bytes_down INTEGER NOT NULL DEFAULT 0;
