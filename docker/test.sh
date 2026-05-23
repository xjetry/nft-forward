#!/usr/bin/env bash
# Smoke tests for the single-image compose fixture.
# Asserts: stack comes up, panel HTTP is reachable, daemon unix socket
# is accessible through the shared volume, stack tears down cleanly.
set -euo pipefail

cd "$(dirname "$0")"

green() { printf '\033[32m%s\033[0m\n' "$*"; }
note()  { printf '\033[36m> %s\033[0m\n' "$*"; }
fail()  { printf '\033[31m失败：%s\033[0m\n' "$*"; exit 1; }

note "1. 启动 daemon + server"
docker compose up -d
trap 'note "清理"; docker compose down -v' EXIT

note "2. 等待 panel 就绪 (http://localhost:8080/healthz)"
for i in $(seq 1 30); do
  curl -sf http://localhost:8080/healthz >/dev/null 2>&1 && break
  sleep 1
done
curl -sf http://localhost:8080/healthz >/dev/null \
  || fail "panel /healthz 超时未就绪"
green "  panel 就绪"

note "3. 通过 daemon 容器访问 unix socket"
docker compose exec daemon \
  curl --unix-socket /var/run/nft-forward.sock \
       -sf http://daemon/v1/health \
  | grep -q '"ok":true' \
  || fail "daemon unix socket 健康检查未返回 {\"ok\":true}"
green "  daemon unix socket 健康正常"

note "5. update 子命令 dry-run（语法 + URL 替换）"
docker compose exec daemon bash -c '
  set -e
  # Sanity: usage text 含 update
  /usr/local/sbin/nft-forward-install --help | grep -q "update" \
    || { echo "usage 缺 update"; exit 1; }
  # 错误用法：--release 与 update 互斥
  if /usr/local/sbin/nft-forward-install update --release v1.0 2>&1 \
     | grep -q "update 只拉 latest"; then
    echo "  --release guard 生效"
  else
    echo "  --release guard 未生效"; exit 1
  fi
  # 错误用法：--purge 仅 uninstall 有效
  if /usr/local/sbin/nft-forward-install update --purge 2>&1 \
     | grep -q "--purge 仅 uninstall 模式有效"; then
    echo "  --purge guard 生效"
  else
    echo "  --purge guard 未生效"; exit 1
  fi
  # NFTF_RELEASE_BASE_URL 接管：mock release artifact
  mkdir -p /tmp/relmock
  cp /usr/local/sbin/nft-forward /tmp/relmock/nft-forward
  ( cd /tmp/relmock && sha256sum nft-forward > SHA256SUMS )
  ( cd /tmp/relmock && python3 -m http.server 8765 >/tmp/http.log 2>&1 & disown )
  for _ in $(seq 1 20); do
    curl -sf http://127.0.0.1:8765/nft-forward -o /tmp/check.bin && break
    sleep 0.2
  done
  test -s /tmp/check.bin \
    || { echo "mock http server 未就绪（4s 内）"; cat /tmp/http.log; exit 1; }
  test "$(sha256sum /tmp/check.bin | awk "{print \$1}")" \
       = "$(sha256sum /usr/local/sbin/nft-forward | awk "{print \$1}")" \
    || { echo "URL 替换/sha 校验链路异常"; exit 1; }
  # Sanity: 确认 install.sh 源码里仍保留 NFTF_RELEASE_BASE_URL 的 dispatch 分支
  grep -q "NFTF_RELEASE_BASE_URL" /usr/local/sbin/nft-forward-install \
    || { echo "install.sh 已丢失 NFTF_RELEASE_BASE_URL dispatch"; exit 1; }
' || fail "step 5 失败"
green "  update 子命令 dry-run 通过（systemd 依赖见手工章节）"

note "6. uninstall --purge 参数 guards"
docker compose exec daemon bash -c '
  /usr/local/sbin/nft-forward-install tui --purge 2>&1 \
    | grep -q "--purge 仅 uninstall 模式有效" \
    || { echo "tui --purge guard 未生效"; exit 1; }
' || fail "step 6 失败"
green "  uninstall guard 验证通过"

note "8. shim: DOCKER-USER 同步与清理"
docker compose exec daemon bash -c '
  set -e
  # 容器里手工建 DOCKER-USER chain 模拟 Docker 主机环境
  nft add table ip filter 2>/dev/null || true
  nft add chain ip filter DOCKER-USER 2>/dev/null || true

  # 提交一条 DNAT 规则到 daemon
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST -H "Content-Type: application/json" \
       http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[{\"id\":\"a\",\"proto\":\"tcp\",\"src_port\":58443,\"dest_ip\":\"10.20.1.20\",\"dest_port\":8443}]}" \
       >/dev/null

  # 验证 DOCKER-USER 里出现 nft-forward managed rule
  if ! nft list chain ip filter DOCKER-USER | grep -q "nft-forward managed"; then
    echo "shim 未注入 DOCKER-USER"
    nft list chain ip filter DOCKER-USER
    exit 1
  fi
  if ! nft list chain ip filter DOCKER-USER | grep -q "10.20.1.20"; then
    echo "DNAT 目标 IP 未出现在 shim 规则中"
    exit 1
  fi
  echo "  注入验证通过"

  # 提交空 ruleset，触发 shim 同步删除
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X POST -H "Content-Type: application/json" \
       http://daemon/v1/ruleset/tui \
       -d "{\"rules\":[]}" \
       >/dev/null

  # 此时 ct state 兜底规则应仍在（每次 sync 都会重写一条），但 dest_ip 那条应消失
  if nft list chain ip filter DOCKER-USER | grep -q "10.20.1.20"; then
    echo "shim 未清除 10.20.1.20 对应的 accept rule"
    exit 1
  fi
  echo "  同步删除验证通过"
' || fail "step 8 失败"
green "  shim 注入与同步验证通过"

note "7. 停止并清理 (compose down -v)"
# EXIT trap handles compose down -v; disable it to avoid double-run and
# run explicitly so the exit code from compose down is visible.
trap - EXIT
docker compose down -v
green "  清理完成"

echo
green "===== compose smoke test 通过 ====="
