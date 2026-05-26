-- Reshape the nodes table for the agent-dialer model:
--   * local_migrated_at: server-side idempotency anchor for register_local
--   * last_seen / online / agent_version: replace the periodic-poller view
--     of node liveness with a hub-driven one (updated on hello + heartbeat)
--   * node_kind: distinguish the panel's built-in self-node from remote
--     agents so dispatch can short-circuit to the unix socket
--
-- The legacy dirty/last_apply_at/last_seen_at/last_error columns from
-- the push-based pusher.go remain in place to keep the migration small;
-- queries.go simply stops reading them. A later cleanup migration can
-- drop them once we're confident no rollback path needs them.

ALTER TABLE nodes ADD COLUMN local_migrated_at INTEGER;
ALTER TABLE nodes ADD COLUMN last_seen         INTEGER;
ALTER TABLE nodes ADD COLUMN online            INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN agent_version     TEXT;
ALTER TABLE nodes ADD COLUMN node_kind         TEXT NOT NULL DEFAULT 'remote';

-- Partial unique index on the self-node so UpsertSelfNode can use ON CONFLICT.
CREATE UNIQUE INDEX idx_nodes_self ON nodes(node_kind) WHERE node_kind = 'self';

CREATE TABLE node_tui_snapshot (
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  forwards_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
