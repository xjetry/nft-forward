-- Allow tcp+udp as a valid proto value in forwards and chains tables.
-- SQLite does not support ALTER CONSTRAINT, so we rebuild both tables.

PRAGMA foreign_keys = OFF;

-- forwards: widen proto CHECK to include 'tcp+udp'
CREATE TABLE forwards_new (
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
  mode TEXT NOT NULL DEFAULT 'kernel',
  chain_id INTEGER REFERENCES chains(id) ON DELETE CASCADE,
  UNIQUE(node_id, proto, listen_port)
);
INSERT INTO forwards_new SELECT * FROM forwards;
DROP TABLE forwards;
ALTER TABLE forwards_new RENAME TO forwards;
CREATE INDEX idx_forwards_node ON forwards(node_id);

-- chains: widen proto CHECK to include 'tcp+udp'
CREATE TABLE chains_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  exit_host TEXT NOT NULL,
  exit_port INTEGER NOT NULL,
  entry_node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
INSERT INTO chains_new SELECT * FROM chains;
DROP TABLE chains;
ALTER TABLE chains_new RENAME TO chains;

PRAGMA foreign_keys = ON;
