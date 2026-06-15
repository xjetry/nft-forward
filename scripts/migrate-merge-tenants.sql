-- Merge tenants into users: each tenant's quota/traffic/expiry fields move onto
-- its first login user, and all owner references (forwards, chains, grants)
-- switch from tenant_id to user_id (renamed owner_id on forwards/chains).
--
-- Idempotent: skipped when the tenants table no longer exists.
-- Run against a stopped panel; the Go layer must match the post-migration schema.

-- Guard: if tenants table is already gone, the migration was already applied.
SELECT CASE
  WHEN (SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tenants') = 0
  THEN RAISE(ABORT, 'tenants table absent — migration already applied')
END;

PRAGMA foreign_keys = OFF;

BEGIN TRANSACTION;

-- 1. Build tenant→user mapping (each tenant's first login user by MIN(id)).
CREATE TEMP TABLE _tenant_user_map AS
  SELECT tenant_id, MIN(id) AS user_id
  FROM users
  WHERE tenant_id IS NOT NULL
  GROUP BY tenant_id;

-- 2. Recreate users WITHOUT tenant_id, WITH quota/traffic/expiry columns.
CREATE TABLE users_new (
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

INSERT INTO users_new (id, username, pw_hash, role, disabled, disable_reason, max_forwards, traffic_quota_bytes, traffic_used_bytes, expires_at, created_at)
  SELECT
    u.id,
    u.username,
    u.pw_hash,
    CASE WHEN u.role = 'tenant' THEN 'user' ELSE u.role END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN COALESCE(t.disabled, 0)
      ELSE u.disabled
    END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN t.disable_reason
      ELSE NULL
    END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN COALESCE(t.max_forwards, 0)
      ELSE 0
    END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN COALESCE(t.traffic_quota_bytes, 0)
      ELSE 0
    END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN COALESCE(t.traffic_used_bytes, 0)
      ELSE 0
    END,
    CASE
      WHEN u.role = 'tenant' AND u.tenant_id IS NOT NULL AND m.user_id = u.id
        THEN t.expires_at
      ELSE NULL
    END,
    u.created_at
  FROM users u
  LEFT JOIN _tenant_user_map m ON m.tenant_id = u.tenant_id
  LEFT JOIN tenants t ON t.id = u.tenant_id;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- 3. Recreate forwards with owner_id instead of tenant_id.
CREATE TABLE forwards_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
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

INSERT INTO forwards_new (id, node_id, owner_id, tunnel_id, proto, listen_port, target_ip, target_port, comment, disabled, last_bytes, total_bytes, created_at, mode, chain_id)
  SELECT
    f.id, f.node_id,
    m.user_id,
    f.tunnel_id, f.proto, f.listen_port, f.target_ip, f.target_port,
    f.comment, f.disabled, f.last_bytes, f.total_bytes, f.created_at, f.mode, f.chain_id
  FROM forwards f
  LEFT JOIN _tenant_user_map m ON m.tenant_id = f.tenant_id;

DROP TABLE forwards;
ALTER TABLE forwards_new RENAME TO forwards;
CREATE INDEX idx_forwards_node ON forwards(node_id);

-- 4. Recreate chains with owner_id instead of tenant_id.
CREATE TABLE chains_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  owner_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  exit_host TEXT NOT NULL,
  exit_port INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

INSERT INTO chains_new (id, owner_id, name, proto, exit_host, exit_port, entry_node_id, entry_listen_port, created_at)
  SELECT
    c.id,
    m.user_id,
    c.name, c.proto, c.exit_host, c.exit_port,
    c.entry_node_id, c.entry_listen_port, c.created_at
  FROM chains c
  LEFT JOIN _tenant_user_map m ON m.tenant_id = c.tenant_id;

DROP TABLE chains;
ALTER TABLE chains_new RENAME TO chains;
CREATE INDEX idx_chains_owner ON chains(owner_id);

-- 5. Create user_tunnels from tenant_tunnels with remapped IDs.
CREATE TABLE user_tunnels (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tunnel_id INTEGER NOT NULL REFERENCES tunnels(id) ON DELETE CASCADE,
  max_forwards INTEGER NOT NULL DEFAULT 10,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY(user_id, tunnel_id)
);

INSERT INTO user_tunnels (user_id, tunnel_id, max_forwards, granted_at)
  SELECT m.user_id, tt.tunnel_id, tt.max_forwards, tt.granted_at
  FROM tenant_tunnels tt
  JOIN _tenant_user_map m ON m.tenant_id = tt.tenant_id;

-- 6. Create user_tunnel_combos from tenant_tunnel_combos with remapped IDs.
CREATE TABLE user_tunnel_combos (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  combo_id INTEGER NOT NULL REFERENCES tunnel_combos(id) ON DELETE CASCADE,
  max_forwards INTEGER NOT NULL DEFAULT 10,
  granted_at INTEGER NOT NULL,
  PRIMARY KEY(user_id, combo_id)
);

INSERT INTO user_tunnel_combos (user_id, combo_id, max_forwards, granted_at)
  SELECT m.user_id, ttc.combo_id, ttc.max_forwards, ttc.granted_at
  FROM tenant_tunnel_combos ttc
  JOIN _tenant_user_map m ON m.tenant_id = ttc.tenant_id;

-- 7. Drop old tables.
DROP TABLE tenant_tunnels;
DROP TABLE tenant_tunnel_combos;
DROP TABLE tenants;

-- 8. Clean up temp table.
DROP TABLE _tenant_user_map;

COMMIT;

PRAGMA foreign_keys = ON;
