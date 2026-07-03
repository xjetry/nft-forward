-- A node with no_direct_exit set may not terminate a rule chain: it cannot
-- launch the exit segment itself, so a rule entering here must cascade into
-- at least one more middle layer. Enforced server-side when deriving chains.
ALTER TABLE nodes ADD COLUMN no_direct_exit INTEGER NOT NULL DEFAULT 0;
