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

ALTER TABLE forwards ADD COLUMN tenant_id INTEGER REFERENCES tenants(id);
ALTER TABLE forwards ADD COLUMN tunnel_id INTEGER REFERENCES tunnels(id);
ALTER TABLE forwards ADD COLUMN last_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE forwards ADD COLUMN total_bytes INTEGER NOT NULL DEFAULT 0;
