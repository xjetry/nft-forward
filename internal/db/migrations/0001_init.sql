CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  pw_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('admin','user')),
  disabled INTEGER NOT NULL DEFAULT 0,
  disable_reason TEXT,
  max_forwards INTEGER NOT NULL DEFAULT 0,
  traffic_quota_bytes INTEGER NOT NULL DEFAULT 0,
  traffic_used_bytes INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,
  created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE nodes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  node_type TEXT NOT NULL DEFAULT 'remote' CHECK(node_type IN ('remote','self','composite')),
  owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  address TEXT NOT NULL DEFAULT '',
  secret TEXT NOT NULL DEFAULT '',
  relay_host TEXT NOT NULL DEFAULT '',
  online INTEGER NOT NULL DEFAULT 0,
  agent_version TEXT NOT NULL DEFAULT '',
  last_seen INTEGER,
  last_apply_at INTEGER,
  last_error TEXT,
  disabled INTEGER NOT NULL DEFAULT 0,
  local_migrated_at INTEGER,
  port_range TEXT NOT NULL DEFAULT '10001-20000',
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_nodes_self ON nodes(node_type) WHERE node_type = 'self';

CREATE TABLE node_hops (
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  hop_node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  PRIMARY KEY (node_id, position)
);

CREATE TABLE user_nodes (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  max_forwards INTEGER NOT NULL DEFAULT 10,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY(user_id, node_id)
);

CREATE TABLE rules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  name TEXT NOT NULL DEFAULT '',
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  exit_host TEXT NOT NULL,
  exit_port INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  comment TEXT NOT NULL DEFAULT '',
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_rules_node ON rules(node_id);
CREATE INDEX idx_rules_owner ON rules(owner_id);

CREATE TABLE rule_hops (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  rule_id INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  listen_port INTEGER NOT NULL CHECK(listen_port BETWEEN 1 AND 65535),
  target_host TEXT NOT NULL,
  target_port INTEGER NOT NULL CHECK(target_port BETWEEN 1 AND 65535),
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  comment TEXT NOT NULL DEFAULT '',
  last_bytes INTEGER NOT NULL DEFAULT 0,
  total_bytes INTEGER NOT NULL DEFAULT 0,
  UNIQUE(node_id, proto, listen_port)
);
CREATE INDEX idx_rule_hops_node ON rule_hops(node_id);
CREATE INDEX idx_rule_hops_rule ON rule_hops(rule_id);

CREATE TABLE settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER,
  action TEXT NOT NULL,
  target TEXT,
  payload TEXT,
  at INTEGER NOT NULL
);


-- Mark prior migrations as already applied so adding them as separate files
-- later won't re-run on DBs created from this consolidated init.
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES ('0004_simplify_schema.sql', strftime('%s','now'));
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES ('0005_node_port_range.sql', strftime('%s','now'));
