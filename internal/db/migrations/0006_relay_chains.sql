-- 节点数据面可达地址：当该节点处于中继链路里时，上一跳 DNAT/relay 打过去的目标。
-- 空 = 从未进过链路；进链路前由 handler 校验必填。与 nodes.address（控制面，agent
-- 反向拨入，无可靠数据面 host）区分。
ALTER TABLE nodes ADD COLUMN relay_host TEXT NOT NULL DEFAULT '';

-- 一条中继链路 = 从自动分配的入口端点、经 N 个受管节点、到自由填写的出口的有序转发链。
-- tenant_id NULL => 管理员链路（不计量、端口在高位段自由分配）；非 NULL => 租户链路
-- （每跳落在租户已授权 tunnel 内，复用 tunnel 的端口段/CIDR/带宽/配额）。
CREATE TABLE chains (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id         INTEGER REFERENCES tenants(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  proto             TEXT NOT NULL CHECK(proto IN ('tcp','udp')),
  exit_host         TEXT NOT NULL,
  exit_port         INTEGER NOT NULL CHECK(exit_port BETWEEN 1 AND 65535),
  entry_node_id     INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
  entry_listen_port INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_chains_tenant ON chains(tenant_id);

-- 每跳一行，按 position 升序（0 = 入口跳）。tunnel_id：admin 链路 NULL，租户链路为该跳
-- 取端口/约束所依据的 granted tunnel。listen_port 为在 node_id 上分配的端口；mode 为该跳数据面。
CREATE TABLE chain_hops (
  chain_id    INTEGER NOT NULL REFERENCES chains(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  tunnel_id   INTEGER REFERENCES tunnels(id) ON DELETE CASCADE,
  listen_port INTEGER NOT NULL,
  mode        TEXT NOT NULL DEFAULT 'kernel' CHECK(mode IN ('kernel','userspace')),
  PRIMARY KEY (chain_id, position)
);
CREATE INDEX idx_chain_hops_node ON chain_hops(node_id);

-- 给链路自动生成的 forward 打标记，使链路能按 chain_id 整条重算/删除。一跳拥有恰好一条 forward。
ALTER TABLE forwards ADD COLUMN chain_id INTEGER REFERENCES chains(id) ON DELETE CASCADE;
