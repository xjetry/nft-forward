-- rules.exit_uri (0010): user proxy URIs moved client-side; never read or
-- written since, so every row holds the '' default.
-- rules.bandwidth_mbps (0026): per-rule shaping was replaced by the per-grant
-- rate limit on user_nodes; the column stopped being projected or updated.
-- nodes.local_migrated_at (0001): nothing ever stamped it, so it is NULL on
-- every deployment; local-rule migration is tracked by the daemon clearing
-- its own "tui" segment instead.
ALTER TABLE rules DROP COLUMN exit_uri;
ALTER TABLE rules DROP COLUMN bandwidth_mbps;
ALTER TABLE nodes DROP COLUMN local_migrated_at;
