#!/usr/bin/env bash
# migrate_v2.sh — simplify schema: merge tunnels/combos into nodes,
# merge forwards/chains into rules.
#
# Usage:  bash migrate_v2.sh [/path/to/panel.db]
# Default DB: /var/lib/nft-forward/panel.db
#
# Run ONCE on a stopped panel. Back up is created automatically.
set -euo pipefail

DB="${1:-/var/lib/nft-forward/panel.db}"

if [ ! -f "$DB" ]; then
  echo "ERROR: database not found: $DB" >&2
  exit 1
fi

# Already migrated?
if sqlite3 "$DB" "SELECT 1 FROM sqlite_master WHERE type='table' AND name='rules'" 2>/dev/null | grep -q 1; then
  echo "ERROR: 'rules' table already exists — migration already applied?" >&2
  exit 1
fi

# Backup
BACKUP="${DB}.pre-v2-$(date +%Y%m%d%H%M%S).bak"
cp "$DB" "$BACKUP"
echo "backed up → $BACKUP"

echo "running migration …"
sqlite3 "$DB" <<'SQL'
-- ================================================================
-- v2 schema migration: simplify entities
-- ================================================================
-- tunnels + tunnel_combos  → nodes (composite)
-- forwards + chains        → rules + rule_hops
-- user_tunnels + user_tunnel_combos → user_nodes

PRAGMA foreign_keys = OFF;
BEGIN TRANSACTION;

-- ────────────────────────────────────────────
-- 1. Recreate nodes: node_kind → node_type, drop dirty/last_seen_at
-- ────────────────────────────────────────────
CREATE TABLE nodes_new (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  node_type TEXT NOT NULL DEFAULT 'remote'
    CHECK(node_type IN ('remote','self','composite')),
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
  created_at INTEGER NOT NULL
);

INSERT INTO nodes_new (id, name, node_type, address, secret, relay_host,
  online, agent_version, last_seen, last_apply_at, last_error, disabled,
  local_migrated_at, created_at)
SELECT id, name, node_kind, address, secret, relay_host,
  online, COALESCE(agent_version,''), last_seen, last_apply_at, last_error,
  disabled, local_migrated_at, created_at
FROM nodes;

DROP TABLE nodes;
ALTER TABLE nodes_new RENAME TO nodes;
CREATE UNIQUE INDEX idx_nodes_self ON nodes(node_type) WHERE node_type = 'self';

-- ────────────────────────────────────────────
-- 2. Create new tables
-- ────────────────────────────────────────────
CREATE TABLE node_hops (
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  hop_node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  PRIMARY KEY (node_id, position)
);

CREATE TABLE user_nodes (
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id  INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
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
  rule_id     INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  proto TEXT NOT NULL CHECK(proto IN ('tcp','udp','tcp+udp')),
  listen_port INTEGER NOT NULL CHECK(listen_port BETWEEN 1 AND 65535),
  target_host TEXT NOT NULL,
  target_port INTEGER NOT NULL CHECK(target_port BETWEEN 1 AND 65535),
  mode TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  comment TEXT NOT NULL DEFAULT '',
  last_bytes  INTEGER NOT NULL DEFAULT 0,
  total_bytes INTEGER NOT NULL DEFAULT 0,
  UNIQUE(node_id, proto, listen_port)
);
CREATE INDEX idx_rule_hops_node ON rule_hops(node_id);
CREATE INDEX idx_rule_hops_rule ON rule_hops(rule_id);

-- ────────────────────────────────────────────
-- 3. tunnel_combos → composite nodes + node_hops
-- ────────────────────────────────────────────
CREATE TEMP TABLE _combo_to_node AS
  SELECT
    tc.id AS combo_id,
    tc.name AS combo_name,
    tc.created_at,
    (SELECT COALESCE(MAX(id),0) FROM nodes)
      + ROW_NUMBER() OVER (ORDER BY tc.id) AS new_node_id
  FROM tunnel_combos tc;

INSERT INTO nodes (id, name, node_type, created_at)
  SELECT
    new_node_id,
    CASE
      WHEN (SELECT COUNT(*) FROM _combo_to_node WHERE combo_name = c.combo_name) > 1
        OR EXISTS (SELECT 1 FROM nodes WHERE name = c.combo_name)
      THEN c.combo_name || '-' || c.combo_id
      ELSE c.combo_name
    END,
    'composite',
    c.created_at
  FROM _combo_to_node c;

INSERT INTO node_hops (node_id, position, hop_node_id, mode)
  SELECT ctn.new_node_id, tch.position, t.node_id, tch.mode
  FROM tunnel_combo_hops tch
  JOIN tunnels t ON t.id = tch.tunnel_id
  JOIN _combo_to_node ctn ON ctn.combo_id = tch.combo_id;

-- ────────────────────────────────────────────
-- 4. Match multi-hop chains to composite nodes
-- ────────────────────────────────────────────
CREATE TEMP TABLE _chain_hop_count AS
  SELECT chain_id, COUNT(*) AS cnt FROM chain_hops GROUP BY chain_id;

CREATE TEMP TABLE _composite_hop_count AS
  SELECT node_id AS composite_id, COUNT(*) AS cnt
  FROM node_hops GROUP BY node_id;

-- position-by-position match: same (node, mode) at every position,
-- and both sides have the same number of hops
CREATE TEMP TABLE _chain_composite_match AS
  SELECT ch.chain_id, nh.node_id AS composite_id
  FROM chain_hops ch
  JOIN node_hops nh
    ON nh.position = ch.position
   AND nh.hop_node_id = ch.node_id
   AND nh.mode = ch.mode
  JOIN _chain_hop_count   chc ON chc.chain_id    = ch.chain_id
  JOIN _composite_hop_count nhc ON nhc.composite_id = nh.node_id
  WHERE chc.cnt > 1   -- multi-hop chains only
  GROUP BY ch.chain_id, nh.node_id
  HAVING COUNT(*) = chc.cnt AND COUNT(*) = nhc.cnt;

-- pick first match per chain (lowest composite_id)
CREATE TEMP TABLE _chain_composite_final AS
  SELECT chain_id, MIN(composite_id) AS composite_id
  FROM _chain_composite_match
  GROUP BY chain_id;

-- ────────────────────────────────────────────
-- 5. Unmatched multi-hop chains → new composite nodes
-- ────────────────────────────────────────────
CREATE TEMP TABLE _unmatched_chains AS
  SELECT chc.chain_id
  FROM _chain_hop_count chc
  WHERE chc.cnt > 1
    AND chc.chain_id NOT IN (SELECT chain_id FROM _chain_composite_final);

CREATE TEMP TABLE _unmatched_to_node AS
  SELECT
    uc.chain_id,
    ch.name  AS chain_name,
    ch.created_at,
    (SELECT COALESCE(MAX(id),0) FROM nodes)
      + ROW_NUMBER() OVER (ORDER BY uc.chain_id) AS new_node_id
  FROM _unmatched_chains uc
  JOIN chains ch ON ch.id = uc.chain_id;

INSERT INTO nodes (id, name, node_type, created_at)
  SELECT
    new_node_id,
    CASE
      WHEN (SELECT COUNT(*) FROM _unmatched_to_node WHERE chain_name = u.chain_name) > 1
        OR EXISTS (SELECT 1 FROM nodes WHERE name = u.chain_name)
      THEN u.chain_name || '-c' || u.chain_id
      ELSE u.chain_name
    END,
    'composite',
    u.created_at
  FROM _unmatched_to_node u;

INSERT INTO node_hops (node_id, position, hop_node_id, mode)
  SELECT utn.new_node_id, ch.position, ch.node_id, ch.mode
  FROM chain_hops ch
  JOIN _unmatched_to_node utn ON utn.chain_id = ch.chain_id;

-- combined chain → composite mapping
CREATE TEMP TABLE _chain_to_composite AS
  SELECT chain_id, composite_id FROM _chain_composite_final
  UNION ALL
  SELECT chain_id, new_node_id FROM _unmatched_to_node;

-- ────────────────────────────────────────────
-- 6. Create rules — assign IDs deterministically
-- ────────────────────────────────────────────

-- every chain → rule (ID = row number in chain-id order)
CREATE TEMP TABLE _chain_rule_map AS
  SELECT id AS chain_id,
    ROW_NUMBER() OVER (ORDER BY id) AS rule_id
  FROM chains;

-- every standalone forward → rule (IDs continue after chains)
CREATE TEMP TABLE _fwd_rule_map AS
  SELECT id AS forward_id,
    (SELECT COUNT(*) FROM chains)
      + ROW_NUMBER() OVER (ORDER BY id) AS rule_id
  FROM forwards
  WHERE chain_id IS NULL;

-- 6a. multi-hop chain rules → composite node
INSERT INTO rules (id, node_id, owner_id, name, proto,
  exit_host, exit_port, entry_listen_port, comment, disabled, created_at)
SELECT
  crm.rule_id,
  ctc.composite_id,
  ch.owner_id,
  ch.name, ch.proto, ch.exit_host, ch.exit_port, ch.entry_listen_port,
  '', 0, ch.created_at
FROM chains ch
JOIN _chain_rule_map crm      ON crm.chain_id = ch.id
JOIN _chain_to_composite ctc  ON ctc.chain_id = ch.id;

-- 6b. single-hop chain rules → physical node
INSERT INTO rules (id, node_id, owner_id, name, proto,
  exit_host, exit_port, entry_listen_port, comment, disabled, created_at)
SELECT
  crm.rule_id,
  chop.node_id,
  ch.owner_id,
  ch.name, ch.proto, ch.exit_host, ch.exit_port, ch.entry_listen_port,
  '', 0, ch.created_at
FROM chains ch
JOIN _chain_rule_map crm     ON crm.chain_id = ch.id
JOIN _chain_hop_count chc    ON chc.chain_id = ch.id AND chc.cnt = 1
JOIN chain_hops chop         ON chop.chain_id = ch.id AND chop.position = 0
WHERE ch.id NOT IN (SELECT chain_id FROM _chain_to_composite);

-- 6c. standalone forward rules → physical node
INSERT INTO rules (id, node_id, owner_id, name, proto,
  exit_host, exit_port, entry_listen_port, comment, disabled, created_at)
SELECT
  frm.rule_id,
  f.node_id,
  f.owner_id,
  '', f.proto, f.target_ip, f.target_port, f.listen_port,
  f.comment, f.disabled, f.created_at
FROM forwards f
JOIN _fwd_rule_map frm ON frm.forward_id = f.id;

-- ────────────────────────────────────────────
-- 7. Create rule_hops from forwards
-- ────────────────────────────────────────────

-- 7a. chain-owned forwards → rule_hops
--     join through chain_hops to get position
INSERT INTO rule_hops (rule_id, position, node_id, proto,
  listen_port, target_host, target_port, mode, comment,
  last_bytes, total_bytes)
SELECT
  crm.rule_id,
  chop.position,
  f.node_id,
  f.proto, f.listen_port, f.target_ip, f.target_port, f.mode,
  CASE WHEN chop.comment <> '' THEN chop.comment ELSE f.comment END,
  f.last_bytes, f.total_bytes
FROM forwards f
JOIN _chain_rule_map crm ON crm.chain_id = f.chain_id
JOIN chain_hops chop
  ON  chop.chain_id    = f.chain_id
  AND chop.node_id     = f.node_id
  AND chop.listen_port = f.listen_port;

-- 7b. standalone forwards → single rule_hop at position 0
INSERT INTO rule_hops (rule_id, position, node_id, proto,
  listen_port, target_host, target_port, mode, comment,
  last_bytes, total_bytes)
SELECT
  frm.rule_id,
  0,
  f.node_id,
  f.proto, f.listen_port, f.target_ip, f.target_port, f.mode,
  f.comment, f.last_bytes, f.total_bytes
FROM forwards f
JOIN _fwd_rule_map frm ON frm.forward_id = f.id;

-- ────────────────────────────────────────────
-- 8. Migrate user grants → user_nodes
-- ────────────────────────────────────────────

-- from user_tunnels (tunnel → physical node)
INSERT INTO user_nodes (user_id, node_id, max_forwards, granted_at)
  SELECT ut.user_id, t.node_id, ut.max_forwards, ut.granted_at
  FROM user_tunnels ut
  JOIN tunnels t ON t.id = ut.tunnel_id
  ON CONFLICT(user_id, node_id) DO UPDATE SET
    max_forwards = MAX(user_nodes.max_forwards, excluded.max_forwards);

-- from user_tunnel_combos (combo → composite node)
INSERT INTO user_nodes (user_id, node_id, max_forwards, granted_at)
  SELECT utc.user_id, ctn.new_node_id, utc.max_forwards, utc.granted_at
  FROM user_tunnel_combos utc
  JOIN _combo_to_node ctn ON ctn.combo_id = utc.combo_id
  ON CONFLICT(user_id, node_id) DO UPDATE SET
    max_forwards = MAX(user_nodes.max_forwards, excluded.max_forwards);

-- ────────────────────────────────────────────
-- 9. Flush TUI snapshot cache (format changed)
-- ────────────────────────────────────────────
DELETE FROM node_tui_snapshot;

-- ────────────────────────────────────────────
-- 10. Drop old tables
-- ────────────────────────────────────────────
DROP TABLE IF EXISTS user_tunnel_combos;
DROP TABLE IF EXISTS user_tunnels;
DROP TABLE IF EXISTS tunnel_combo_hops;
DROP TABLE IF EXISTS tunnel_combos;
DROP TABLE IF EXISTS chain_hops;
DROP TABLE IF EXISTS forwards;
DROP TABLE IF EXISTS chains;
DROP TABLE IF EXISTS tunnels;
DROP TABLE IF EXISTS node_tui_snapshot;

-- ────────────────────────────────────────────
-- 11. Add owner_id to nodes (v0.13.0)
-- ────────────────────────────────────────────
ALTER TABLE nodes ADD COLUMN owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
UPDATE nodes SET owner_id = (
  SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1
) WHERE node_type = 'self';

-- ────────────────────────────────────────────
-- 12. Record migration
-- ────────────────────────────────────────────
INSERT OR IGNORE INTO schema_migrations(version, applied_at)
  VALUES ('0004_simplify_schema.sql', strftime('%s','now'));

COMMIT;
PRAGMA foreign_keys = ON;
SQL

# ── verify ──
echo ""
echo "=== verification ==="
sqlite3 "$DB" <<'VERIFY'
SELECT '  tables:  ' || GROUP_CONCAT(name, ', ')
FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '__%';
SELECT '  nodes:   ' || COUNT(*) || ' (composite: ' ||
  SUM(CASE WHEN node_type='composite' THEN 1 ELSE 0 END) || ')' FROM nodes;
SELECT '  node_hops: ' || COUNT(*) FROM node_hops;
SELECT '  rules:   ' || COUNT(*) FROM rules;
SELECT '  rule_hops: ' || COUNT(*) FROM rule_hops;
SELECT '  user_nodes: ' || COUNT(*) FROM user_nodes;
SELECT '  [gone] tunnels:  ' ||
  CASE WHEN (SELECT COUNT(*) FROM sqlite_master WHERE name='tunnels')=0
    THEN 'dropped' ELSE 'STILL EXISTS' END;
SELECT '  [gone] chains:   ' ||
  CASE WHEN (SELECT COUNT(*) FROM sqlite_master WHERE name='chains')=0
    THEN 'dropped' ELSE 'STILL EXISTS' END;
SELECT '  [gone] forwards: ' ||
  CASE WHEN (SELECT COUNT(*) FROM sqlite_master WHERE name='forwards')=0
    THEN 'dropped' ELSE 'STILL EXISTS' END;
VERIFY

echo ""
echo "done. backup at: $BACKUP"
