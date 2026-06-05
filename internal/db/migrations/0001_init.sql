-- nft-forward schema — single-file init (no incremental migrations)

CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  pw_hash TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('admin','tenant')),
  tenant_id INTEGER,
  disabled INTEGER NOT NULL DEFAULT 0,
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
  address TEXT NOT NULL DEFAULT '',
  secret TEXT NOT NULL,
  last_seen_at INTEGER,
  last_apply_at INTEGER,
  last_error TEXT,
  dirty INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  local_migrated_at INTEGER,
  last_seen INTEGER,
  online INTEGER NOT NULL DEFAULT 0,
  agent_version TEXT,
  node_kind TEXT NOT NULL DEFAULT 'remote',
  relay_host TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_nodes_self ON nodes(node_kind) WHERE node_kind = 'self';

CREATE TABLE tenants (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  max_forwards INTEGER NOT NULL DEFAULT 100,
  traffic_quota_bytes INTEGER NOT NULL DEFAULT 0,
  traffic_used_bytes INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER,
  disabled INTEGER NOT NULL DEFAULT 0,
  disable_reason TEXT,
  created_at INTEGER NOT NULL
);

CREATE TABLE tunnels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  proto_mask TEXT NOT NULL DEFAULT 'tcp+udp' CHECK(proto_mask IN ('tcp','udp','tcp+udp')),
  port_start INTEGER NOT NULL CHECK(port_start BETWEEN 1 AND 65535),
  port_end INTEGER NOT NULL CHECK(port_end BETWEEN 1 AND 65535),
  target_cidr_allow TEXT NOT NULL DEFAULT '0.0.0.0/0',
  bandwidth_mbps INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(node_id, name),
  CHECK(port_end >= port_start)
);
CREATE INDEX idx_tunnels_node ON tunnels(node_id);

CREATE TABLE tenant_tunnels (
  tenant_id INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  tunnel_id INTEGER NOT NULL REFERENCES tunnels(id) ON DELETE CASCADE,
  max_forwards INTEGER NOT NULL DEFAULT 10,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY(tenant_id, tunnel_id)
);

CREATE TABLE forwards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  tenant_id INTEGER REFERENCES tenants(id) ON DELETE SET NULL,
  tunnel_id INTEGER REFERENCES tunnels(id) ON DELETE NO ACTION,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  listen_port INTEGER NOT NULL CHECK(listen_port BETWEEN 1 AND 65535),
  target_ip TEXT NOT NULL,
  target_port INTEGER NOT NULL CHECK(target_port BETWEEN 1 AND 65535),
  comment TEXT NOT NULL DEFAULT '',
  disabled INTEGER NOT NULL DEFAULT 0,
  last_bytes INTEGER NOT NULL DEFAULT 0,
  total_bytes INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  chain_id INTEGER REFERENCES chains(id) ON DELETE CASCADE,
  UNIQUE(node_id, proto, listen_port)
);
CREATE INDEX idx_forwards_node ON forwards(node_id);

CREATE TABLE chains (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  exit_host TEXT NOT NULL,
  exit_port INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_chains_tenant ON chains(tenant_id);

CREATE TABLE chain_hops (
  chain_id INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  tunnel_id INTEGER REFERENCES tunnels(id) ON DELETE CASCADE,
  listen_port INTEGER NOT NULL,
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  comment TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (chain_id, position)
);
CREATE INDEX idx_chain_hops_node ON chain_hops(node_id);

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

CREATE TABLE node_tui_snapshot (
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  forwards_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
