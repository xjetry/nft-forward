#!/usr/bin/env bash
# nft-forward 一键安装脚本
# 从 GitHub release 下载 nft-server / nft-agent 二进制并配置 server / agent / tui / uninstall
# 用法：见 --help

set -euo pipefail

REPO="xjetry/nft-forward"
RELEASE="${NFTF_RELEASE:-latest}"
INSTALL_DIR="/usr/local/sbin"
SYSTEMD_DIR="/etc/systemd/system"
ETC_DIR="/etc/nft-forward"
SCRIPT_PATH="$INSTALL_DIR/nft-forward-upgrade"
GH_PROXY_FILE="$ETC_DIR/gh-proxy"
_update_tmp=""

# gh-proxy is a URL prefix concatenated in front of the full GitHub URL, e.g.
# https://gh-proxy.com/https://github.com/owner/repo/... . Empty = direct.
# Resolution order: --gh-proxy / NFTF_GH_PROXY env / persisted file. Persisting
# it means later self-upgrades (update / update-script / the upgrade wrapper)
# keep reaching GitHub through the same mirror without re-passing the flag.
GH_PROXY="${NFTF_GH_PROXY:-}"
GH_PROXY_EXPLICIT=""

normalize_gh_proxy() {
  if [[ -n "$GH_PROXY" && "$GH_PROXY" != */ ]]; then
    GH_PROXY="$GH_PROXY/"
  fi
}

# raw.githubusercontent base for the script itself (self / update-script).
script_url() {
  echo "${GH_PROXY}https://raw.githubusercontent.com/$REPO/main/install.sh"
}

write_daemon_unit() {
  local extra_args="${1:-}"
  cat >"$SYSTEMD_DIR/nft-forward-daemon.service" <<EOF
[Unit]
Description=nft-forward host daemon (nftables controller)
After=network-online.target nftables.service
Wants=network-online.target

[Service]
ExecStart=$INSTALL_DIR/nft-agent daemon$extra_args
Restart=on-failure
RuntimeDirectory=nft-forward
RuntimeDirectoryMode=0750
StateDirectory=nft-forward
StateDirectoryMode=0750

[Install]
WantedBy=multi-user.target
EOF
}

write_server_unit() {
  local addr="${1:-:7788}"
  cat >"$SYSTEMD_DIR/nft-forward-server.service" <<EOF
[Unit]
Description=nft-forward web panel
Requires=nft-forward-daemon.service
After=nft-forward-daemon.service

[Service]
ExecStart=$INSTALL_DIR/nft-server --addr $addr
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
}

remove_legacy_units() {
  # Earlier releases installed three separate units (nft-forward.service for
  # the standalone TUI apply path, nft-server.service for the panel, and
  # nft-agent.service for remote nodes). They are replaced by the
  # daemon-first layout above. Disable and remove any we find so systemctl
  # status reflects the new world.
  for unit in nft-forward.service nft-server.service nft-agent.service; do
    if [[ -f "$SYSTEMD_DIR/$unit" ]]; then
      systemctl disable --now "$unit" 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/$unit"
    fi
  done

  # The single-binary era shipped one `nft-forward` that every unit invoked
  # via a subcommand. The split layout uses separate nft-server / nft-agent
  # binaries, so drop the stale combined binary to keep PATH from shadowing
  # them with a binary that no longer understands the new unit ExecStarts.
  rm -f "$INSTALL_DIR/nft-forward"
}

# Detect what role is currently installed by reading systemd unit-files and
# the daemon unit ExecStart. Echoes a space-separated list of roles found
# (any of "server" "agent"), or nothing if only a baseline daemon-only TUI
# install exists. Caller is expected to dispatch into uninstall paths for
# each role echoed.
detect_existing_roles() {
  local roles=()
  if [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]]; then
    roles+=(server)
  fi
  if [[ -f "$SYSTEMD_DIR/nft-forward-daemon.service" ]] \
     && grep -q -- '--connect' "$SYSTEMD_DIR/nft-forward-daemon.service"; then
    roles+=(agent)
  fi
  # Use an if-block (not `&&` short-circuit) so an empty array doesn't
  # make this function return non-zero, which would propagate into
  # `existing=$(detect_existing_roles)` and trip `set -e` silently.
  if [[ ${#roles[@]} -gt 0 ]]; then
    echo "${roles[*]}"
  fi
}

# When installing mode $new, clean up any conflicting old roles.
# The matrix:
#   tui    -> uninstall server, uninstall agent
#   server -> uninstall agent  (server unit gets rewritten in place)
#   agent  -> uninstall server (daemon unit gets rewritten with --connect)
# Re-installing the same role doesn't trigger cleanup.
switch_role_cleanup() {
  local new="$1"
  local existing
  existing="$(detect_existing_roles)"
  # Use explicit if-blocks (not `&&` short-circuit) — a case branch
  # ending in a failed `[[ … ]] && …` returns nonzero, which would make
  # this function return nonzero and trip `set -e` in the caller on a
  # fresh host where `existing` is empty.
  case "$new" in
    tui)
      if [[ "$existing" == *server* ]]; then do_uninstall server 0; fi
      if [[ "$existing" == *agent*  ]]; then do_uninstall agent  0; fi
      ;;
    server)
      if [[ "$existing" == *agent* ]]; then do_uninstall agent 0; fi
      ;;
    agent)
      if [[ "$existing" == *server* ]]; then do_uninstall server 0; fi
      ;;
  esac
}

# Inline uninstall implementation: target is server/agent/daemon, purge is 0/1.
# Called both from the top-level "mode=uninstall" path and from
# switch_role_cleanup (which always passes purge=0).
do_uninstall() {
  local target="$1"
  local purge="${2:-0}"
  case "$target" in
    server)
      systemctl disable --now nft-forward-server.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-server.service"
      # The panel host's local node daemon runs nft-agent; the standalone
      # nft-server binary is panel-only, so it can go when the role does.
      rm -f "$INSTALL_DIR/nft-server"
      systemctl daemon-reload
      if [[ "$purge" -eq 1 ]]; then
        # Clear daemon's panel segment so leftover rules from server pushes
        # don't keep forwarding after the panel database is gone.
        curl -sf --unix-socket /var/run/nft-forward.sock \
             -X POST -H 'Content-Type: application/json' \
             http://daemon/v1/ruleset/panel \
             -d '{"rules":[]}' >/dev/null 2>&1 \
          || echo "警告: 未能通过 daemon API 清 panel 段（daemon 可能已停）" >&2
        rm -f /var/lib/nft-forward/panel.db \
              /var/lib/nft-forward/panel.db-wal \
              /var/lib/nft-forward/panel.db-shm
        ok "已卸载 server 角色 + 清 panel.db 与 daemon panel 段"
      else
        ok "已卸载 server 角色（daemon 保留；panel.db 与 daemon panel 段保留）"
      fi
      ;;
    agent)
      if [[ "$purge" -eq 1 ]]; then
        curl -sf --unix-socket /var/run/nft-forward.sock \
             -X POST -H 'Content-Type: application/json' \
             http://daemon/v1/ruleset/panel \
             -d '{"rules":[]}' >/dev/null 2>&1 \
          || echo "警告: 未能通过 daemon API 清 panel 段（daemon 可能已停）" >&2
      else
        # Migrate panel segment back into tui segment so live forwards survive.
        curl -sf --unix-socket /var/run/nft-forward.sock \
             -X POST http://daemon/v1/admin/demote-to-tui >/dev/null 2>&1 \
          || echo "警告: 未能通过 daemon API 降级 panel→tui 段（daemon 可能已停）" >&2
      fi
      write_daemon_unit ""
      rm -f /etc/nft-forward/panel.token
      systemctl daemon-reload
      systemctl restart nft-forward-daemon.service
      if [[ "$purge" -eq 1 ]]; then
        rm -rf /etc/nft-forward/
        ok "已卸载 agent 角色 + 清 /etc/nft-forward/ 与 daemon panel 段"
      else
        ok "已卸载 agent 角色（daemon 保留；panel 段已迁回 tui 段，token 文件已删）"
      fi
      ;;
    daemon)
      if systemctl is-active --quiet nft-forward-server.service \
         || [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]]; then
        die "请先卸载 server 角色：sudo $0 uninstall server"
      fi
      systemctl disable --now nft-forward-daemon.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-daemon.service"
      rm -f "$INSTALL_DIR/nft-agent"
      systemctl daemon-reload
      if [[ "$purge" -eq 1 ]]; then
        rm -rf /var/lib/nft-forward/
        rm -f /etc/sysctl.d/99-nft-forward.conf
        # Best-effort: drop the dedicated nftables table if still loaded.
        nft delete table ip nft_forward 2>/dev/null || true
        # Drop the system group only if it exists; ignore errors otherwise.
        if getent group nft-forward >/dev/null; then
          groupdel nft-forward 2>/dev/null || true
        fi
        ok "已卸载 daemon + 清 state.json / sysctl drop-in / nftables 表 / 系统组"
        echo "提示：如有 tc HTB 限速残留，请手动 tc qdisc del dev <iface> root" >&2
      else
        ok "已卸载 daemon（state.json 保留在 /var/lib/nft-forward/）"
      fi
      ;;
    all)
      systemctl disable --now nft-forward-server.service 2>/dev/null || true
      systemctl disable --now nft-forward-daemon.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-server.service" \
            "$SYSTEMD_DIR/nft-forward-daemon.service"
      rm -f "$INSTALL_DIR/nft-server" "$INSTALL_DIR/nft-agent" \
            "$INSTALL_DIR/nft-forward-upgrade"
      systemctl daemon-reload
      nft delete table ip nft_forward 2>/dev/null || true
      rm -f /etc/sysctl.d/99-nft-forward.conf
      rm -rf /etc/nft-forward/
      # 保留 panel.db，清其他 state
      rm -f /var/lib/nft-forward/state.json
      if getent group nft-forward >/dev/null; then
        groupdel nft-forward 2>/dev/null || true
      fi
      ok "已卸载所有角色（panel.db 保留在 /var/lib/nft-forward/）"
      ;;
    *) die "未知卸载目标: $target" ;;
  esac
}

usage() {
  cat <<USAGE
nft-forward 一键安装/卸载/升级脚本（nft-server 面板 + nft-agent 节点）

用法:
  $0 [tui|server|agent|update|update-script|uninstall|reset-password] [选项]

模式:
  tui              单机 TUI（装 nft-agent；host daemon 已被自动安装为 systemd 服务）
  server           控制面板（装 nft-server + nft-agent；依赖 daemon；自动叠加安装）
  agent            受控节点（装 nft-agent；daemon 主动 dial panel WebSocket 反向纳管）
  update           拉 latest 二进制原子替换 + restart + 失败回滚（按已装角色拉对应二进制）
  update-script    从 GitHub main 分支拉取最新 install.sh 覆盖本地升级脚本
  uninstall <角色> 卸载指定角色（server / agent / daemon / all）；daemon 单独卸载前请先卸 server/agent
  reset-password   重置面板 admin 密码（仅限装了 server 的机器；自动停/起 server）

选项 / 环境变量:
  --panel-url URL  (PANEL_URL)    agent 连向的 panel 地址（http(s)://… 或 ws(s)://…）
  --token TOKEN    (AGENT_TOKEN)   agent bearer token（agent 模式必填）
  --port-range R                   agent 占用的中继端口范围，格式 START-END（默认 10001-20000）
  --addr ADDR      (PANEL_ADDR)    server 监听地址；默认 :7788
  --release VER    (NFTF_RELEASE)  GitHub release tag，默认 latest（update 模式禁用）
  --gh-proxy PFX   (NFTF_GH_PROXY) GitHub 镜像前缀（如 https://gh-proxy.com/）；
                                   留空 = 直连。安装时持久化，后续自升级自动沿用
  --purge                          uninstall 模式专用：按角色 scope 清残留数据
  --password PW                    reset-password 模式：新密码（缺省则交互输入或随机生成）
  -h, --help                       显示此帮助

二进制:
  nft-server  面板（web 前端 embed + sqlite + chi）；装于 $INSTALL_DIR/nft-server
  nft-agent   节点（daemon + TUI）；装于 $INSTALL_DIR/nft-agent
              daemon: nft-agent daemon [--connect …]；TUI: nft-agent

示例:
  sudo $0                                # 交互式
  sudo $0 server --addr :9000            # 自定义面板端口
  sudo $0 agent --panel-url https://panel.example.com --token abc...  # 远程节点
  sudo $0 agent --panel-url https://panel.example.com --token abc... --gh-proxy https://gh-proxy.com/
  sudo $0 update                         # 拉 latest 二进制升级
  sudo $0 update-script                  # 更新本地升级脚本（不动二进制）
  sudo $0 uninstall server               # 仅卸面板，保留 daemon
  sudo $0 uninstall daemon --purge       # 完整擦除 daemon 残留
  sudo $0 reset-password                 # 交互重置面板 admin 密码
  sudo nft-forward-upgrade               # 等效于 sudo $0 update（安装后可用）
USAGE
}

die() { echo "错误: $*" >&2; exit 1; }
note() { printf '\033[36m%s\033[0m\n' "$*"; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }

# Persist the chosen gh-proxy so later self-upgrades reuse it. Empty value
# leaves an empty file (treated as "direct"), keeping a single source of truth.
persist_gh_proxy() {
  mkdir -p "$ETC_DIR"
  printf '%s' "$GH_PROXY" >"$GH_PROXY_FILE"
}

persist_script() {
  if curl -fsSL "$(script_url)" -o "$tmp/upgrade.sh" 2>/dev/null; then
    install -m 0755 "$tmp/upgrade.sh" "$SCRIPT_PATH"
    note "升级脚本已保存到 $SCRIPT_PATH（后续升级: sudo nft-forward-upgrade）"
  fi
}

do_update_script() {
  local stmp surl
  surl="$(script_url)"
  stmp="$(mktemp -d)"
  note "下载最新 install.sh ..."
  curl -fsSL "$surl" -o "$stmp/upgrade.sh" \
    || { rm -rf "$stmp"; die "下载失败: $surl"; }
  install -m 0755 "$stmp/upgrade.sh" "$SCRIPT_PATH"
  rm -rf "$stmp"
  ok "升级脚本已更新: $SCRIPT_PATH"
}

# Resolve "latest" to the concrete tag so identity files record a real version
# rather than the moving "latest" alias. Falls back to "latest" only if the
# GitHub redirect can't be read (e.g. restricted network without a proxy set).
resolve_release_tag() {
  if [[ "$RELEASE" != "latest" ]]; then
    echo "$RELEASE"
    return 0
  fi
  local url="${GH_PROXY}https://github.com/$REPO/releases/latest"
  local resolved
  resolved="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url" 2>/dev/null \
              | sed -n 's#.*/releases/tag/##p' | tr -d '\r\n')"
  if [[ -n "$resolved" ]]; then
    echo "$resolved"
  else
    echo "latest"
  fi
}

# Write the nft-agent identity files consumed by the daemon's first hello so the
# panel knows which version/sha the node is running before the daemon self-
# computes its sha. MUST run on every install and every upgrade path that lays
# down an nft-agent binary; missing it leaves a node misreporting its identity.
#   $1 = version label, $2 = sha256 hex of the installed nft-agent binary
write_agent_identity() {
  local version="$1" sha="$2"
  mkdir -p "$ETC_DIR"
  printf '%s' "$version" >"$ETC_DIR/agent.version"
  printf '%s' "$sha" >"$ETC_DIR/agent.sha"
}

# Download one release asset through the proxy and strong-verify it against the
# release's SHA256SUMS. Echoes the verified sha256 hex on success.
#   $1 = base URL (releases/download root), $2 = asset name, $3 = dest path
#   $4 = "strict" to hard-fail when SHA256SUMS is unavailable (binaries), or
#        "soft" to warn and skip (legacy tolerance for optional fetches)
fetch_and_verify() {
  local base="$1" asset="$2" dest="$3" strictness="${4:-strict}"
  curl -fL --progress-bar "$base/$asset" -o "$dest" \
    || die "下载失败: $base/$asset"
  local ddir
  ddir="$(dirname "$dest")"
  if curl -fLs "$base/SHA256SUMS" -o "$ddir/SHA256SUMS" 2>/dev/null; then
    # Verification chatter must go to stderr — stdout carries only the hash the
    # caller captures into the identity file.
    (cd "$ddir" && grep -E "  $asset\$" SHA256SUMS | sha256sum -c - >&2) \
      || die "sha256 校验失败: $asset"
  elif [[ "$strictness" == "strict" ]]; then
    die "未取到 SHA256SUMS：必须强校验，拒绝裸跑（检查网络/代理或稍后重试）"
  else
    echo "    (SHA256SUMS 不可用，跳过校验)" >&2
  fi
  sha256sum "$dest" | awk '{print $1}'
}

rollback_update() {
  echo "update 失败，回滚到旧二进制" >&2
  for bin in nft-server nft-agent; do
    if [[ -f "$INSTALL_DIR/$bin.bak" ]]; then
      mv -f "$INSTALL_DIR/$bin.bak" "$INSTALL_DIR/$bin"
    fi
  done
  systemctl restart nft-forward-daemon.service 2>/dev/null || true
  if [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]]; then
    systemctl restart nft-forward-server.service 2>/dev/null || true
  fi
  exit 1
}

do_update() {
  # ---- 前置探测 ----
  # Probe the unit file write_daemon_unit actually writes, not
  # `systemctl list-unit-files`: its column layout varies across systemd
  # versions and can fail to match an installed unit, falsely reporting a
  # working install as missing.
  [[ -f "$SYSTEMD_DIR/nft-forward-daemon.service" ]] \
    || die "未安装：nft-forward-daemon.service 不存在；请先 install.sh tui/server/agent"
  [[ -x "$INSTALL_DIR/nft-agent" ]] \
    || die "未安装：$INSTALL_DIR/nft-agent 不存在；请先 install.sh tui/server/agent"

  # A server install always carries nft-server alongside nft-agent; pull both
  # so the panel and its local node daemon advance in lockstep.
  local want_server=0
  [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]] && want_server=1

  # ---- 下载到 tmp ----
  _update_tmp="$(mktemp -d)"
  trap 'rm -rf "$_update_tmp"' EXIT

  local tag agent_sha
  tag="$(resolve_release_tag)"

  note "[1/5] 下载二进制 ($tag) ..."
  agent_sha="$(fetch_and_verify "$base" nft-agent "$_update_tmp/nft-agent" strict)"
  if [[ "$want_server" -eq 1 ]]; then
    fetch_and_verify "$base" nft-server "$_update_tmp/nft-server" strict >/dev/null
  fi

  # No product-side arch probe here. The host arch is already gated by the
  # uname -m check before any mode runs, and the sha256 above pins this
  # download to the published amd64 asset byte-for-byte — together those
  # already guarantee the binary's architecture. Whether the binary can
  # actually run is proven by the post-install health-check (a daemon that
  # fails to start rolls the swap back automatically).

  # ---- 备份旧二进制 ----
  note "[2/5] 备份旧二进制到 $INSTALL_DIR/*.bak ..."
  cp -a "$INSTALL_DIR/nft-agent" "$INSTALL_DIR/nft-agent.bak"
  if [[ "$want_server" -eq 1 && -x "$INSTALL_DIR/nft-server" ]]; then
    cp -a "$INSTALL_DIR/nft-server" "$INSTALL_DIR/nft-server.bak"
  fi
  trap 'rm -rf "$_update_tmp"; rollback_update' ERR INT TERM

  # ---- 原子替换 ----
  note "[3/5] 原子替换二进制 ..."
  install -m 0755 "$_update_tmp/nft-agent" "$INSTALL_DIR/nft-agent"
  if [[ "$want_server" -eq 1 ]]; then
    install -m 0755 "$_update_tmp/nft-server" "$INSTALL_DIR/nft-server"
  fi
  # Record the freshly-installed nft-agent identity on this upgrade path too.
  write_agent_identity "$tag" "$agent_sha"

  # ---- 重启 unit ----
  note "[4/5] 重启 daemon (+ server, if present) ..."
  systemctl daemon-reload
  systemctl restart nft-forward-daemon.service
  if [[ "$want_server" -eq 1 ]]; then
    systemctl restart nft-forward-server.service
  fi

  # ---- health-check 10 秒预算 ----
  note "[5/5] health-check (10s) ..."
  local i ok_count=0
  for i in $(seq 1 10); do
    if systemctl is-active --quiet nft-forward-daemon.service \
       && curl -sf --unix-socket /var/run/nft-forward.sock \
               http://daemon/v1/health 2>/dev/null \
          | grep -q '"ok":true'; then
      ok_count=1
      break
    fi
    sleep 1
  done
  [[ "$ok_count" -eq 1 ]] || rollback_update

  # ---- 成功收尾 ----
  trap 'rm -rf "$_update_tmp"' EXIT
  trap - ERR INT TERM
  rm -f "$INSTALL_DIR/nft-agent.bak" "$INSTALL_DIR/nft-server.bak"
  ok "===== Update 完成 ====="
  echo "版本标签:       $tag"
  echo "nft-agent sha256: $agent_sha"
  echo "建议查看启动日志: journalctl -u nft-forward-daemon.service --since '1 minute ago'"
}

gen_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 12
  else
    head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

do_reset_password() {
  local db="/var/lib/nft-forward/panel.db"
  [[ -x "$INSTALL_DIR/nft-server" ]] \
    || die "未安装：$INSTALL_DIR/nft-server 不存在；reset-password 仅适用于装了 server 的机器"
  [[ -f "$db" ]] \
    || die "未找到面板数据库 $db；本机似乎未安装 server 角色"

  # 取新密码：--password 优先；否则交互输入两次；直接回车或无 TTY 则随机生成。
  local pw="${RESET_PW:-}" pw2="" generated=0
  if [[ -z "$pw" && -t 0 ]]; then
    read -rsp "输入新的 admin 密码（直接回车=随机生成）: " pw; echo
    if [[ -n "$pw" ]]; then
      read -rsp "再次输入确认: " pw2; echo
      [[ "$pw" == "$pw2" ]] || die "两次输入不一致"
    fi
  fi
  if [[ -z "$pw" ]]; then
    pw="$(gen_password)"
    generated=1
  fi
  [[ "${#pw}" -ge 6 ]] || die "密码至少 6 位"

  # 停 server → 重置 → 重启；daemon 不动，转发不中断。
  local had_server=0
  if [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]]; then
    had_server=1
    note "停止 nft-forward-server.service ..."
    systemctl stop nft-forward-server.service 2>/dev/null || true
  fi

  note "重置 admin 密码 ..."
  if ! "$INSTALL_DIR/nft-server" --reset-admin-password "$pw" --db "$db"; then
    if [[ "$had_server" -eq 1 ]]; then
      systemctl start nft-forward-server.service 2>/dev/null || true
    fi
    die "重置失败（admin 账号不存在？见上方错误）"
  fi

  if [[ "$had_server" -eq 1 ]]; then
    note "重启 nft-forward-server.service ..."
    systemctl start nft-forward-server.service
  fi

  ok "===== 已重置 admin 密码 ====="
  echo "登录用户名: admin"
  if [[ "$generated" -eq 1 ]]; then
    echo "新密码（随机生成，请妥善保存）: $pw"
  fi
  echo "旧会话已全部失效，请用新密码重新登录。"
}

# 当以 nft-forward-upgrade 调用时，默认 update 模式
if [[ "$(basename "$0")" == "nft-forward-upgrade" && $# -eq 0 ]]; then
  set -- update
fi

# 参数解析（--help 不需要 root）
mode=""
panel_url=""
token=""
port_range=""
addr=""
purge=0
RESET_PW=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    tui|server|agent|update|update-script|uninstall|reset-password) mode="$1"; shift ;;
    --panel-url) panel_url="${2:?--panel-url 需要值}"; shift 2 ;;
    --panel-url=*) panel_url="${1#*=}"; shift ;;
    --token) token="${2:?--token 需要值}"; shift 2 ;;
    --token=*) token="${1#*=}"; shift ;;
    --port-range) port_range="${2:?--port-range 需要值}"; shift 2 ;;
    --port-range=*) port_range="${1#*=}"; shift ;;
    --addr) addr="${2:?--addr 需要值}"; shift 2 ;;
    --addr=*) addr="${1#*=}"; shift ;;
    --release) RELEASE="${2:?--release 需要值}"; RELEASE_EXPLICIT=1; shift 2 ;;
    --release=*) RELEASE="${1#*=}"; RELEASE_EXPLICIT=1; shift ;;
    --gh-proxy) GH_PROXY="${2:?--gh-proxy 需要值}"; GH_PROXY_EXPLICIT=1; shift 2 ;;
    --gh-proxy=*) GH_PROXY="${1#*=}"; GH_PROXY_EXPLICIT=1; shift ;;
    --purge) purge=1; shift ;;
    --password) RESET_PW="${2:?--password 需要值}"; shift 2 ;;
    --password=*) RESET_PW="${1#*=}"; shift ;;
    server|agent|daemon)
      if [[ "$mode" == "uninstall" ]]; then UNINSTALL_TARGET="$1"; shift; continue; fi
      die "未知参数: $1" ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

# Fall back to the persisted proxy when neither flag nor env supplied one, so
# self-upgrades (and the upgrade wrapper) keep using the mirror chosen at
# install time. NFTF_GH_PROXY env still seeds GH_PROXY above; only consult the
# file when nothing explicit was given.
if [[ -z "$GH_PROXY_EXPLICIT" && -z "${NFTF_GH_PROXY:-}" && -f "$GH_PROXY_FILE" ]]; then
  GH_PROXY="$(cat "$GH_PROXY_FILE" 2>/dev/null || true)"
fi
normalize_gh_proxy

[[ $EUID -eq 0 ]] || die "请以 root 运行（sudo $0 ...）"

# 架构检测
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) ;;
  *) die "目前仅 amd64 二进制可用（当前: $arch）。请等待后续 release 或自行交叉编译。" ;;
esac

# Mode selection (interactive when no TTY arg).
if [[ -z "$mode" ]]; then
  [[ -t 0 ]] || die "未指定模式且无 TTY；--help 查看用法"
  echo "请选择安装模式:"
  echo "  1) tui             单机 TUI（自动装 daemon）"
  echo "  2) server          控制面板（叠加 daemon）"
  echo "  3) agent           远程节点（daemon 反向 dial panel WebSocket）"
  echo "  4) update          拉 latest 二进制原子升级（保留现有角色）"
  echo "  5) update-script   更新本地升级脚本（不动二进制）"
  echo "  6) uninstall       卸载（再问要卸哪个角色）"
  echo "  7) reset-password  重置面板 admin 密码"
  read -rp "输入数字或名称: " choice
  case "$choice" in
    1|tui)       mode=tui ;;
    2|server)    mode=server ;;
    3|agent)     mode=agent ;;
    4|update)    mode=update ;;
    5|update-script) mode=update-script ;;
    6|uninstall) mode=uninstall ;;
    7|reset-password) mode=reset-password ;;
    *) die "未知选项: $choice" ;;
  esac
fi

if [[ "$mode" == "update" && -n "${RELEASE_EXPLICIT:-}" ]]; then
  die "update 只拉 latest，要锁版本请用 install（如 sudo $0 tui --release v1.2.3）"
fi
if [[ "$mode" != "uninstall" && "$purge" -eq 1 ]]; then
  die "--purge 仅 uninstall 模式有效"
fi

# Per-mode parameter prompts.
case "$mode" in
  agent)
    panel_url="${panel_url:-${PANEL_URL:-}}"
    token="${token:-${AGENT_TOKEN:-}}"
    if [[ -z "$panel_url" && -t 0 ]]; then
      read -rp "Panel URL（如 https://panel.example.com）: " panel_url
    fi
    if [[ -z "$token" && -t 0 ]]; then
      read -rp "Agent bearer token（从面板节点详情页拷贝）: " token
    fi
    [[ -n "$panel_url" ]] || die "agent 模式需要 --panel-url 或 PANEL_URL"
    [[ -n "$token" ]] || die "agent 模式需要 --token 或 AGENT_TOKEN"
    ;;
  server)
    if [[ -z "$addr" && -z "${PANEL_ADDR:-}" && -t 0 ]]; then
      read -rp "面板绑定地址（端口或 地址:端口，默认 :7788）: " _port
      _port="${_port:-7788}"
      if [[ "$_port" == *:* ]]; then
        addr="$_port"
      else
        addr=":$_port"
      fi
    else
      addr="${addr:-${PANEL_ADDR:-:7788}}"
    fi
    ;;
  uninstall)
    if [[ -z "${UNINSTALL_TARGET:-}" && -t 0 ]]; then
      read -rp "卸载哪个角色 [server/agent/daemon/all]: " UNINSTALL_TARGET
    fi
    UNINSTALL_TARGET="${UNINSTALL_TARGET:-}"
    [[ -n "$UNINSTALL_TARGET" ]] || die "uninstall 需要指定角色"
    ;;
esac

# Uninstall takes a separate code path (no download needed).
if [[ "$mode" == "uninstall" ]]; then
  do_uninstall "$UNINSTALL_TARGET" "$purge"
  exit 0
fi

# Reset-password is local-only: no download, just rewrite the admin password in
# the existing panel DB via the installed binary, bouncing the server unit.
if [[ "$mode" == "reset-password" ]]; then
  do_reset_password
  exit 0
fi

# update-script: refresh the local upgrade script only, no binary change.
if [[ "$mode" == "update-script" ]]; then
  do_update_script
  exit 0
fi

# Update is its own code path: no role unit changes, only binary swap.
if [[ "$mode" == "update" ]]; then
  if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
    base="$NFTF_RELEASE_BASE_URL"
  else
    base="${GH_PROXY}https://github.com/$REPO/releases/latest/download"
  fi
  do_update
  exit 0
fi

# All install modes need binaries + the daemon unit. Resolve the release base,
# download/verify the role's binaries, then layer the role-specific unit on top.
remove_legacy_units

if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
  base="$NFTF_RELEASE_BASE_URL"
elif [[ "$RELEASE" == "latest" ]]; then
  base="${GH_PROXY}https://github.com/$REPO/releases/latest/download"
else
  base="${GH_PROXY}https://github.com/$REPO/releases/download/$RELEASE"
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Concrete tag for the agent.version identity file (resolves "latest").
release_tag="$(resolve_release_tag)"

# Every role installs nft-agent (the node daemon + TUI); only server adds the
# panel binary on top.
note "[1/3] 下载 nft-agent ($RELEASE) ..."
agent_sha="$(fetch_and_verify "$base" nft-agent "$tmp/nft-agent" strict)"
if [[ "$mode" == "server" ]]; then
  note "      下载 nft-server ($RELEASE) ..."
  fetch_and_verify "$base" nft-server "$tmp/nft-server" strict >/dev/null
fi

note "[2/3] 安装到 $INSTALL_DIR ..."
install -m 0755 "$tmp/nft-agent" "$INSTALL_DIR/nft-agent"
if [[ "$mode" == "server" ]]; then
  install -m 0755 "$tmp/nft-server" "$INSTALL_DIR/nft-server"
fi

note "[3/3] 写入身份文件 + 持久化升级脚本/代理 ..."
write_agent_identity "$release_tag" "$agent_sha"
persist_gh_proxy
persist_script

primary_ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
primary_ip="${primary_ip:-<本机IP>}"

case "$mode" in
  tui)
    switch_role_cleanup tui
    write_daemon_unit ""
    systemctl daemon-reload
    systemctl enable --now nft-forward-daemon.service
    cat <<EOF

$(ok "===== TUI 安装完成 =====")
host daemon 已作为 systemd 服务启动：nft-forward-daemon.service
运行 TUI:   sudo $INSTALL_DIR/nft-agent
TUI 会自动连接到上面的 daemon；规则在 daemon 重启后自动恢复。

文档:  https://github.com/$REPO#readme
EOF
    ;;

  server)
    switch_role_cleanup server
    write_daemon_unit ""
    write_server_unit "$addr"
    systemctl daemon-reload
    systemctl enable --now nft-forward-daemon.service
    systemctl enable --now nft-forward-server.service
    sleep 2
    _admin_pw="$(journalctl -u nft-forward-server.service --since '30 seconds ago' --no-pager -o cat 2>/dev/null \
                 | sed -n 's/.*密  码: *//p' | head -1)"
    echo ""
    ok "===== Server 安装完成 ====="
    if [[ "$addr" == :* ]]; then
      echo "面板:        http://$primary_ip$addr"
    else
      echo "面板:        http://$addr"
    fi
    echo "daemon unit: nft-forward-daemon.service (nft-agent daemon)"
    echo "server unit: nft-forward-server.service (nft-server)"
    if [[ -n "$_admin_pw" ]]; then
      echo ""
      echo "管理员账号:  admin"
      echo "管理员密码:  $_admin_pw"
      echo ""
      echo "请妥善保存密码！可通过面板修改或 install.sh reset-password 重置。"
    else
      echo ""
      echo "非首次安装，admin 密码沿用之前设置的密码。"
      echo "如需重置: sudo $0 reset-password"
    fi
    ;;

  agent)
    switch_role_cleanup agent
    # Normalize URL: http→ws, https→wss; append /v1/agents if path empty.
    panel_url="${panel_url:-${PANEL_URL:-}}"
    [[ -n "$panel_url" ]] || die "agent 模式需要 --panel-url 或 PANEL_URL"
    case "$panel_url" in
      https://*) panel_url="wss://${panel_url#https://}" ;;
      http://*)  panel_url="ws://${panel_url#http://}" ;;
      wss://*|ws://*) ;;
      *) die "panel-url 必须以 http(s):// 或 ws(s):// 开头" ;;
    esac
    case "$panel_url" in
      *"/v1/agents"|*"/v1/agents/") ;;
      */) panel_url="${panel_url}v1/agents" ;;
      *)  panel_url="${panel_url}/v1/agents" ;;
    esac
    mkdir -p /etc/nft-forward
    install -m 0600 /dev/stdin /etc/nft-forward/panel.token <<<"$token"
    range_arg=""
    [[ -n "$port_range" ]] && range_arg=" --port-range $port_range"
    write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token${range_arg}"
    systemctl daemon-reload
    # enable --now 对已运行的 daemon 是 no-op，不会重启；装 agent 时 daemon 往往
    # 已在运行（纯 daemon 段或先前角色），必须 restart 才能让新写入的 --connect
    # 参数真正生效，否则旧进程继续以无 --connect 的纯 daemon 模式运行、永不上线。
    systemctl enable nft-forward-daemon.service
    systemctl restart nft-forward-daemon.service
    cat <<EOF

$(ok "===== Agent 安装完成 =====")
daemon 已通过 WebSocket 连向 $panel_url
本机不再暴露任何 HTTP 端口给 panel；如要排查，查看
  journalctl -u nft-forward-daemon.service -f

文档:  https://github.com/$REPO#readme
EOF
    ;;

  *) die "内部错误: 未处理的模式 $mode" ;;
esac
