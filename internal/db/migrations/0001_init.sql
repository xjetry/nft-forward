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
  address TEXT NOT NULL,
  secret TEXT NOT NULL,
  last_seen_at INTEGER,
  last_apply_at INTEGER,
  last_error TEXT,
  dirty INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE forwards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  listen_port INTEGER NOT NULL CHECK(listen_port BETWEEN 1 AND 65535),
  target_ip TEXT NOT NULL,
  target_port INTEGER NOT NULL CHECK(target_port BETWEEN 1 AND 65535),
  comment TEXT NOT NULL DEFAULT '',
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(node_id, proto, listen_port)
);
CREATE INDEX idx_forwards_node ON forwards(node_id);

CREATE TABLE audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER,
  action TEXT NOT NULL,
  target TEXT,
  payload TEXT,
  at INTEGER NOT NULL
);
