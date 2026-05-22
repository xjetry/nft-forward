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

note "4. 停止并清理 (compose down -v)"
# EXIT trap handles compose down -v; disable it to avoid double-run and
# run explicitly so the exit code from compose down is visible.
trap - EXIT
docker compose down -v
green "  清理完成"

echo
green "===== compose smoke test 通过 ====="
