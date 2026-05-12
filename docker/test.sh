#!/usr/bin/env bash
# 端到端测试：1 server + 3 agents
# 覆盖：admin 推送 / 节点持久化 / 多租户校验 / 流量计数 + 配额自动禁用 / 用户禁用-删除级联
set -euo pipefail

cd "$(dirname "$0")"

BASE="http://localhost:18080"
ADMIN_COOKIE="/tmp/nftf-admin.cookies"
ALICE_COOKIE="/tmp/nftf-alice.cookies"
T1="token_agent1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
T2="token_agent2_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
T3="token_agent3_cccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
note()  { printf '\033[36m> %s\033[0m\n' "$*"; }
fail()  { red "失败：$*"; exit 1; }

cleanup() { rm -f "$ADMIN_COOKIE" "$ALICE_COOKIE"; }
trap cleanup EXIT

# 通用 POST helpers，使用 cookie jar 自动跟 303 转 GET（curl 默认行为，无 -X POST）
post() {
  local jar="$1" url="$2"; shift 2
  local args=()
  while (( $# >= 2 )); do args+=( --data-urlencode "$1=$2" ); shift 2; done
  if (( ${#args[@]} == 0 )); then
    curl -sfS -b "$jar" -L -d "" "$url" -o /dev/null
  else
    curl -sfS -b "$jar" -L "${args[@]}" "$url" -o /dev/null
  fi
}
admin() { post "$ADMIN_COOKIE" "$@"; }
alice() { post "$ALICE_COOKIE" "$@"; }
alice_reject() {
  local url="$1"; shift
  local args=()
  while (( $# >= 2 )); do args+=( --data-urlencode "$1=$2" ); shift 2; done
  # 我们的服务器对租户校验失败仍返回 303 redirect + flash cookie，
  # 所以 -f 不会触发。改为检查副作用即可（不返回错误码）。
  curl -sS -b "$ALICE_COOKIE" -L "${args[@]}" "$url" -o /dev/null
}

rules_on() { docker exec "$1" nft list table ip nft_forward 2>/dev/null || true; }

wait_until() {
  local label="$1"; local timeout="$2"; shift 2
  for ((i=0; i<timeout; i++)); do
    if "$@"; then return 0; fi
    sleep 1
  done
  return 1
}

# === A. 等待容器就绪 ===
note "A. 等待 server + 3 个 agent 就绪"
for i in {1..30}; do curl -sf "$BASE/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "$BASE/healthz" >/dev/null || fail "server 未就绪"
for c in nftf-agent1 nftf-agent2 nftf-agent3; do
  wait_until "$c" 30 docker exec "$c" curl -sf http://localhost:7878/healthz >/dev/null 2>&1 || fail "$c 未就绪"
done
green "  全部就绪"

# === B. admin 登录 ===
note "B. admin 登录"
curl -sfS -c "$ADMIN_COOKIE" -L -d "username=admin&password=admin123" "$BASE/login" -o /dev/null
grep -q nft_session "$ADMIN_COOKIE" || fail "admin 登录失败"
green "  admin 已登录"

# === C. 注册 3 个节点（预共享 token） ===
note "C. 注册 3 个 agent 节点（localhost 节点由 server 内嵌 agent 自动创建为 #1）"
admin "$BASE/nodes" name agent1 address http://agent1:7878 secret "$T1"
admin "$BASE/nodes" name agent2 address http://agent2:7878 secret "$T2"
admin "$BASE/nodes" name agent3 address http://agent3:7878 secret "$T3"
sleep 2
rules_on nftf-agent1 | grep -q "table ip nft_forward" || fail "agent1 表未建"
# server 自己也应有 nft_forward 表（内嵌 agent 已 bootstrap）
docker exec nftf-server nft list table ip nft_forward 2>&1 | grep -q "table ip nft_forward" || fail "server 自身 nft_forward 表未建"
green "  3 个 agent + localhost 共 4 个节点就绪"

# === D. admin 直接推送：验证 counter 关键字 ===
note "D. admin 直接推一条转发到 agent1，确认 counter 已写入"
admin "$BASE/forwards" node_id 2 proto tcp listen_port 19999 target_ip 10.0.0.1 target_port 22 comment "admin probe"
sleep 2
rules_on nftf-agent1 | grep -q "tcp dport 19999 counter.*dnat to 10.0.0.1:22" || fail "agent1 缺少 admin 推送 (counter)"
green "  agent1 已带 counter 关键字"

# === D2. admin 推一条转发到 localhost (内嵌 agent，进程内调用) ===
note "D2. admin 直接推一条转发到 localhost (内嵌 agent)，验证进程内调用"
admin "$BASE/forwards" node_id 1 proto tcp listen_port 19998 target_ip 10.0.0.2 target_port 22 comment "localhost probe"
sleep 2
docker exec nftf-server nft list table ip nft_forward | grep -q "tcp dport 19998 counter.*dnat to 10.0.0.2:22" || fail "localhost 缺少推送规则"
green "  localhost 节点已通过进程内调用应用规则"

# === E. 创建通道 ===
note "E. 创建通道：t1(agent1,20000-21000), t2(agent2,30000-31000), t3(agent3,40000-41000), t-local(localhost,18000-18999)"
admin "$BASE/tunnels" name t1 node_id 2 proto_mask "tcp+udp" port_start 20000 port_end 21000 target_cidr_allow "10.0.0.0/24" bandwidth_mbps 0
admin "$BASE/tunnels" name t2 node_id 3 proto_mask "tcp"     port_start 30000 port_end 31000 target_cidr_allow "10.0.0.0/24" bandwidth_mbps 0
admin "$BASE/tunnels" name t3 node_id 4 proto_mask "tcp"     port_start 40000 port_end 41000 target_cidr_allow "192.168.0.0/16" bandwidth_mbps 0
admin "$BASE/tunnels" name t-local node_id 1 proto_mask "tcp" port_start 18000 port_end 18999 target_cidr_allow "10.0.0.0/24" bandwidth_mbps 0
green "  通道创建完毕"

# === F. 创建租户 alice + 授权 t1/t2（不授权 t3）+ 账号 ===
note "F. 创建租户 alice + 授权 t1/t2/t-local + 账号"
admin "$BASE/tenants" name alice max_forwards 50 traffic_quota_mb 0
admin "$BASE/tenants/1/grants" tunnel_id 1 max_forwards 5
admin "$BASE/tenants/1/grants" tunnel_id 2 max_forwards 5
admin "$BASE/tenants/1/grants" tunnel_id 4 max_forwards 5
admin "$BASE/tenants/1/users" username alice password alicepw
green "  alice 已建立"

# === G. alice 登录 ===
note "G. alice 登录"
curl -sfS -c "$ALICE_COOKIE" -L -d "username=alice&password=alicepw" "$BASE/login" -o /dev/null
grep -q nft_session "$ALICE_COOKIE" || fail "alice 登录失败"
green "  alice 已登录"

# === H. alice 合法新增 ===
note "H. alice 创建 3 条合法转发（agent1 / agent2 / localhost）+ 1 条留空端口随机分配"
alice "$BASE/my/forwards" tunnel_id 1 proto tcp listen_port 20100 target_ip 10.0.0.5 target_port 80 comment ok-1
alice "$BASE/my/forwards" tunnel_id 2 proto tcp listen_port 30100 target_ip 10.0.0.6 target_port 80 comment ok-2
alice "$BASE/my/forwards" tunnel_id 4 proto tcp listen_port 18100 target_ip 10.0.0.7 target_port 80 comment ok-local
# 留空 listen_port，应在 t1 (20000-21000) 范围内自动分配
alice "$BASE/my/forwards" tunnel_id 1 proto tcp listen_port '' target_ip 10.0.0.9 target_port 90 comment ok-auto
sleep 2
rules_on nftf-agent1 | grep -q "tcp dport 20100 counter.*dnat to 10.0.0.5:80" || fail "alice agent1 未生效"
rules_on nftf-agent2 | grep -q "tcp dport 30100 counter.*dnat to 10.0.0.6:80" || fail "alice agent2 未生效"
docker exec nftf-server nft list table ip nft_forward | grep -q "tcp dport 18100 counter.*dnat to 10.0.0.7:80" || fail "alice localhost 未生效"
# 自动分配的端口应该是 t1 范围内 20000-21000 任意一个且 != 20100
auto_rule=$(rules_on nftf-agent1 | grep -oE "tcp dport [0-9]+ counter.*dnat to 10.0.0.9:90" | head -1)
[[ -n "$auto_rule" ]] || { rules_on nftf-agent1; fail "未找到自动分配端口的规则"; }
auto_port=$(echo "$auto_rule" | grep -oE "dport [0-9]+" | head -1 | awk '{print $2}')
if [[ "$auto_port" -lt 20000 || "$auto_port" -gt 21000 || "$auto_port" -eq 20100 ]]; then
  fail "自动分配端口 $auto_port 不在 20000-21000 或与已用端口冲突"
fi
green "  alice 3 条手工转发 + 自动端口 $auto_port → 10.0.0.9:90 均已生效"

# === I. alice 非法用例：端口超段 / CIDR 不符 / 未授权 tunnel ===
note "I. alice 三种非法用例应被拒"
alice_reject "$BASE/my/forwards" tunnel_id 1 proto tcp listen_port 25000 target_ip 10.0.0.5 target_port 80   # 超段
alice_reject "$BASE/my/forwards" tunnel_id 1 proto tcp listen_port 20200 target_ip 192.168.1.10 target_port 80 # 越 CIDR
alice_reject "$BASE/my/forwards" tunnel_id 3 proto tcp listen_port 40100 target_ip 192.168.1.10 target_port 80 # 未授权
sleep 1
rules_on nftf-agent1 | grep -q "dport 25000" && fail "端口超段竟生效"
rules_on nftf-agent1 | grep -q "192.168.1.10" && fail "CIDR 校验失效"
rules_on nftf-agent3 | grep -qE "dport [0-9]+ counter.*dnat" && fail "未授权 tunnel 竟下发到 agent3"
green "  三种非法用例均被拒"

# === I2. 带宽限速：创建带 5Mbps 的 tunnel + forward → 验证 nft mark + tc HTB ===
note "I2. 创建限速 tunnel(5Mbps) + 授权 + alice 在其下创建转发"
admin "$BASE/tunnels" name t1b node_id 2 proto_mask "tcp" port_start 22000 port_end 22999 target_cidr_allow "10.0.0.0/24" bandwidth_mbps 5
admin "$BASE/tenants/1/grants" tunnel_id 5 max_forwards 5
alice "$BASE/my/forwards" tunnel_id 5 proto tcp listen_port 22100 target_ip 10.0.0.8 target_port 80 comment bw-test
sleep 2
rules_on nftf-agent1 | grep -qE "tcp dport 22100 meta mark set (22100|0x[0-9a-f]+) counter.*dnat to 10.0.0.8:80" || { rules_on nftf-agent1; fail "nft 规则缺 meta mark"; }
docker exec nftf-agent1 tc qdisc show dev eth0 | grep -q "qdisc htb 1: root" || { docker exec nftf-agent1 tc qdisc show dev eth0; fail "tc 缺 htb root qdisc"; }
# 22100 decimal == 0x5654 — tc 把 classid minor 当 hex
docker exec nftf-agent1 tc class show dev eth0 | grep -q "class htb 1:5654" || { docker exec nftf-agent1 tc class show dev eth0; fail "tc 缺 class 1:5654 (=22100)"; }
docker exec nftf-agent1 tc filter show dev eth0 | grep -q "classid 1:5654" || { docker exec nftf-agent1 tc filter show dev eth0; fail "tc 缺 fw filter → 1:5654"; }
green "  ✓ nft meta mark + tc HTB qdisc/class/filter 全部就位"

# === J. 流量计数 + 配额自动禁用 ===
note "J. 设 alice 配额 = 3000 字节，并从 agent2 发 SYN 触发计数"
admin "$BASE/tenants/1/quota-bytes" traffic_quota_bytes 3000
# 让足够多 SYN 包打到 agent1:20100
docker exec nftf-agent2 bash -c 'for i in $(seq 1 400); do curl -m 1 -sf http://agent1:20100 -o /dev/null & done; wait' >/dev/null 2>&1 || true
note "  等待 poller (5s 周期) 累加并触发禁用，最多 25s"
if wait_until "auto-disable" 25 bash -c '! docker exec nftf-agent1 nft list table ip nft_forward 2>/dev/null | grep -q "dport 20100"'; then
  green "  ✓ agent1 上 20100 规则已被清除（alice 自动禁用）"
else
  echo "--- agent1 当前规则 ---"; rules_on nftf-agent1
  echo "--- agent2 当前规则 ---"; rules_on nftf-agent2
  echo "--- server 最近日志 ---"; docker logs nftf-server 2>&1 | tail -30
  fail "超时未触发自动禁用"
fi

# === K. admin 重置流量 → 规则恢复 ===
note "K. admin 重置流量，规则应回到 agent1"
admin "$BASE/tenants/1/reset-traffic"
if wait_until "restore" 10 bash -c 'docker exec nftf-agent1 nft list table ip nft_forward 2>/dev/null | grep -q "tcp dport 20100"'; then
  green "  ✓ 重置后 agent1 上 20100 规则已恢复"
else
  fail "重置后规则未恢复"
fi

# === L. 禁用 alice 的登录账号 → 同步禁用租户（转发应失效） ===
note "L. 禁用 alice 登录账号，验证转发同步失效"
# alice 用户 ID = 2 (admin=1)
admin "$BASE/users/2/toggle"
if wait_until "user-disable" 10 bash -c '! docker exec nftf-agent1 nft list table ip nft_forward 2>/dev/null | grep -q "dport 20100"'; then
  green "  ✓ 禁用账号后 agent1 已清除 alice 的转发"
else
  fail "禁用账号后转发未失效"
fi
# alice 此时也无法登录
status=$(curl -sS -o /dev/null -w '%{http_code}' -c /tmp/nftf-alice-relogin.cookies -L -d "username=alice&password=alicepw" "$BASE/login")
rm -f /tmp/nftf-alice-relogin.cookies
green "  alice 重新登录返回 $status (此时未读取 cookie 内容；账号 disabled 后 cookie 不会被设置)"

# === M. 重新启用 alice → 规则回归 ===
note "M. 重新启用 alice"
admin "$BASE/users/2/toggle"
if wait_until "user-enable" 10 bash -c 'docker exec nftf-agent1 nft list table ip nft_forward 2>/dev/null | grep -q "tcp dport 20100"'; then
  green "  ✓ 重新启用后 agent1 上 20100 规则恢复"
else
  fail "重新启用后规则未恢复"
fi

# === N. 重置密码 ===
note "N. 管理员重置 alice 密码（HTTP 成功即可）"
admin "$BASE/users/2/reset-password"
# alice 此后用原密码登录应失败
status=$(curl -sS -o /dev/null -w '%{http_code}' -c /tmp/nftf-alice-old.cookies -L -d "username=alice&password=alicepw" "$BASE/login")
if grep -q nft_session /tmp/nftf-alice-old.cookies 2>/dev/null; then
  rm -f /tmp/nftf-alice-old.cookies
  fail "重置后旧密码仍可登录"
fi
rm -f /tmp/nftf-alice-old.cookies
green "  ✓ 已重置；旧密码不再可登录"

# === O. 删除 alice 账号 → 唯一账号 → 租户 + 转发一并清除 ===
note "O. 删除 alice 账号，验证租户与转发（agent1 + agent2 + localhost）被释放"
admin "$BASE/users/2/delete"
sleep 2
rules_on nftf-agent1 | grep -q "dport 20100" && fail "agent1 仍有 alice 的 20100"
rules_on nftf-agent2 | grep -q "dport 30100" && fail "agent2 仍有 alice 的 30100"
docker exec nftf-server nft list table ip nft_forward 2>/dev/null | grep -q "dport 18100" && fail "localhost 仍有 alice 的 18100"
# admin 自己的 19999 (agent1) 与 19998 (localhost) 仍应保留
rules_on nftf-agent1 | grep -q "tcp dport 19999 counter.*dnat to 10.0.0.1:22" || fail "admin 自己的 agent1 转发被误删"
docker exec nftf-server nft list table ip nft_forward | grep -q "tcp dport 19998 counter.*dnat to 10.0.0.2:22" || fail "admin 自己的 localhost 转发被误删"
green "  ✓ alice 唯一账号删除 → 租户 + 全部转发清除（含 localhost），admin 自己的转发保留"

# === P. admin 自助改密码（改完再改回，避免影响后续） ===
note "P. admin 自助修改密码"
admin "$BASE/change-password" old_password admin123 new_password newpw9999 confirm newpw9999
curl -sfS -c /tmp/nftf-admin2.cookies -L -d "username=admin&password=newpw9999" "$BASE/login" -o /dev/null
grep -q nft_session /tmp/nftf-admin2.cookies || fail "新密码登录失败"
rm -f /tmp/nftf-admin2.cookies
admin "$BASE/change-password" old_password newpw9999 new_password admin123 confirm admin123
green "  ✓ 自助修改 → 新密码登录 → 改回原密码"

echo
green "===== 多租户端到端测试通过 ====="
