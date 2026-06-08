-- Tunnel combos: pre-defined multi-hop sequences that expand into individual
-- hops when a tenant creates a chain. Admin creates combos, grants them to
-- tenants alongside (or instead of) individual tunnels.

CREATE TABLE tunnel_combos (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE tunnel_combo_hops (
  combo_id INTEGER NOT NULL REFERENCES tunnel_combos(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  tunnel_id INTEGER NOT NULL REFERENCES tunnels(id) ON DELETE CASCADE,
  mode TEXT NOT NULL DEFAULT 'userspace',
  PRIMARY KEY (combo_id, position)
);

CREATE TABLE tenant_tunnel_combos (
  tenant_id INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  combo_id INTEGER NOT NULL REFERENCES tunnel_combos(id) ON DELETE CASCADE,
  max_forwards INTEGER NOT NULL DEFAULT 10,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, combo_id)
);
