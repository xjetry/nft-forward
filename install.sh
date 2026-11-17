#!/usr/bin/env bash
# nft-forward 一键安装脚本
# 从 GitHub release 下载 nft-server / nft-agent 二进制并配置 server / agent / tui / uninstall
# 按本机架构（amd64 / arm64）自动选择对应的发布产物
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

# 让正在运行的 daemon 用上刚写入的单元：重新加载单元、设为开机自启、重启进程。
#
# daemon 是单机、归远程面板（--connect）还是归本机面板（--server-local），完全由
# 单元里 ExecStart 的参数决定，而进程只在启动时读一次参数。`systemctl enable --now`
# 对已经在运行的单元什么都不做，所以每次改写单元之后都要 restart；不重启的话旧进程
# 继续按旧参数运行——该去连面板的不连，该脱离面板的还连着。
restart_daemon() {
  systemctl daemon-reload
  systemctl enable nft-forward-daemon.service
  systemctl restart nft-forward-daemon.service
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

# 清空 daemon 的 panel 段（面板下发的规则）。成功返回 0。
#
# 用的是 daemon 本机控制口上的 DELETE /v1/rules/panel（internal/daemon/handlers.go
# 的 handleClearPanelSegment）：daemon 把这些规则撤出数据面并落盘。它清不掉一个仍
# 连着 daemon 的面板的规则——面板下次下发会填回来——所以只在面板已停、或 daemon
# 马上要脱离面板时调用。
clear_panel_segment() {
  curl -sf --unix-socket /var/run/nft-forward.sock \
       -X DELETE http://daemon/v1/rules/panel >/dev/null 2>&1
}

# Inline uninstall implementation: target is server/agent/daemon, purge is 0/1.
# Called both from the top-level "mode=uninstall" path and from
# switch_role_cleanup (which always passes purge=0).
do_uninstall() {
  local target="$1"
  local purge="${2:-0}"
  local seg_note
  case "$target" in
    server)
      systemctl disable --now nft-forward-server.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-server.service"
      # The panel host's local node daemon runs nft-agent; the standalone
      # nft-server binary is panel-only, so it can go when the role does.
      rm -f "$INSTALL_DIR/nft-server"
      systemctl daemon-reload
      if [[ "$purge" -eq 1 ]]; then
        # 面板数据库删掉之后没有人再管这些规则，所以先把它们从本机 daemon 清掉。
        # 面板服务在上面已经停了，清空之后不会再有下发把它们填回来。
        if clear_panel_segment; then
          seg_note="daemon 的 panel 段已清空"
        else
          seg_note="daemon 的 panel 段未能清空，其中的规则已转为本地规则"
          echo "警告: 未能清空 daemon 的 panel 段（daemon 未运行，或拒绝了请求）；面板下发过的规则会转为本地规则继续转发，可在 TUI 里删除" >&2
        fi
        # 面板连同数据库一起清掉了，不会再回来：daemon 改回单机模式。留着
        # --server-local 的话它会一直去连面板的本机入口 socket。改回单机后，
        # panel 段若没清掉，daemon 启动时会把里面的规则转成本地规则（同下面
        # agent 分支的说明）。
        write_daemon_unit ""
        restart_daemon
        rm -f /var/lib/nft-forward/panel.db \
              /var/lib/nft-forward/panel.db-wal \
              /var/lib/nft-forward/panel.db-shm
        ok "已卸载 server 角色 + 清 panel.db，daemon 改回单机模式；${seg_note}"
      else
        # daemon 保持 --server-local：它留着面板下发的规则继续转发，并等面板装回
        # 来之后重新接入（期间会周期性地尝试连接面板的本机入口并在日志里记失败）。
        ok "已卸载 server 角色（daemon 保留；panel.db 与 daemon panel 段保留）"
      fi
      ;;
    agent)
      # 不带 --purge 时，面板下发的规则要留下来继续转发。这件事不在这里做：下面
      # 把 daemon 改成不带 --connect 再重启，daemon 启动时发现自己是单机模式而
      # panel 段非空，就把 panel 段的规则转成本地规则（internal/daemon/daemon.go
      # 的 Bootstrap）。
      #
      # 带 --purge 时这些规则要去掉，而上述转换恰恰会把它们留下，所以必须赶在
      # 重启之前清空 panel 段。清空与重启之间 daemon 仍连着面板：面板若恰好在这
      # 之间又下发一次，那一批规则会在重启时被转成本地规则留下来。
      seg_note=""
      if [[ "$purge" -eq 1 ]]; then
        if clear_panel_segment; then
          seg_note="daemon 的 panel 段已清空"
        else
          seg_note="daemon 的 panel 段未能清空，其中的规则已转为本地规则"
          echo "警告: 未能清空 daemon 的 panel 段（daemon 未运行，或拒绝了请求）；面板下发过的规则会转为本地规则继续转发，可在 TUI 里删除" >&2
        fi
      fi
      write_daemon_unit ""
      rm -f /etc/nft-forward/panel.token
      restart_daemon
      if [[ "$purge" -eq 1 ]]; then
        rm -rf /etc/nft-forward/
        ok "已卸载 agent 角色 + 清 /etc/nft-forward/；${seg_note}"
      else
        ok "已卸载 agent 角色（daemon 保留；面板下发的规则已转为本地规则，token 文件已删）"
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
  update           拉二进制原子替换 + restart + 失败回滚（默认 latest，可 --release 指定版本）
  update-script    从 GitHub main 分支拉取最新 install.sh 覆盖本地升级脚本
  uninstall <角色> 卸载指定角色（server / agent / daemon / all）；daemon 单独卸载前请先卸 server/agent
  reset-password   重置面板 admin 密码（仅限装了 server 的机器；自动停/起 server）

选项 / 环境变量:
  --panel-url URL  (PANEL_URL)    agent 连向的 panel 地址（http(s)://… 或 ws(s)://…）
  --token TOKEN    (AGENT_TOKEN)   agent bearer token（agent 模式必填）
  --port-range R                   agent 占用的中继端口范围，格式 START-END（默认 10001-20000）
  --relay-host HOST                agent 数据面 IPv4 地址/域名声明，覆盖面板自动识别（双出口中转机场景）
  --relay-host-v6 HOST             agent 数据面 IPv6 地址声明，覆盖面板自动识别
  --addr ADDR      (PANEL_ADDR)    server 监听地址；默认 :7788
  --release VER    (NFTF_RELEASE)  GitHub release tag，默认 latest
  --gh-proxy PFX   (NFTF_GH_PROXY) GitHub 镜像前缀（如 https://gh-proxy.com/）；
                                   留空 = 直连。安装时持久化，后续自升级自动沿用
  --purge                          uninstall 模式专用：按角色 scope 清残留数据
  --password PW                    reset-password 模式：新密码（缺省则交互输入或随机生成）
  --insecure       (NFTF_ALLOW_INSECURE) 允许明文 http/ws 控制信道（有 RCE 风险，仅本地测试）
  -h, --help                       显示此帮助

二进制:
  nft-server  面板（web 前端 embed + sqlite + chi）；装于 $INSTALL_DIR/nft-server
  nft-agent   节点（daemon + TUI）；装于 $INSTALL_DIR/nft-agent
              daemon: nft-agent daemon [--connect …]；TUI: nft-agent
  架构按 uname -m 自动识别：amd64（x86_64）与 arm64（aarch64）各有一套发布产物

示例:
  sudo $0                                # 交互式
  sudo $0 server --addr :9000            # 自定义面板端口
  sudo $0 agent --panel-url https://panel.example.com --token abc...  # 远程节点
  sudo $0 agent --panel-url https://panel.example.com --token abc... --gh-proxy https://gh-proxy.com/
  sudo $0 update                         # 拉 latest 二进制升级
  sudo $0 update --release v0.40.0       # 升级到指定版本
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
warn() { printf '\033[33m警告: %s\033[0m\n' "$*" >&2; }
require_val() {
  if [[ -z "${2:-}" || "${2:-}" == --* ]]; then
    die "$1 需要值（是否漏写了？）"
  fi
}

# Persist the chosen gh-proxy so later self-upgrades reuse it. Empty value
# leaves an empty file (treated as "direct"), keeping a single source of truth.
persist_gh_proxy() {
  mkdir -p "$ETC_DIR"
  printf '%s' "$GH_PROXY" >"$GH_PROXY_FILE"
}

# looks_like_installer sanity-checks a freshly downloaded install.sh before it
# replaces the root-run script. It rejects HTML error pages, truncated bodies,
# and other garbage a proxy/CDN might return. This is NOT full integrity — a
# malicious mirror could still serve a valid-looking script — but it stops the
# common failure mode of installing non-script content as an executable.
looks_like_installer() {
  local f="$1"
  [[ -s "$f" ]] || return 1
  head -1 "$f" | grep -q '^#!' || return 1
  grep -q 'nft-forward' "$f" || return 1
  grep -q 'tui|server|agent' "$f" || return 1
  return 0
}

persist_script() {
  if curl -fsSL "$(script_url)" -o "$tmp/upgrade.sh" 2>/dev/null; then
    if looks_like_installer "$tmp/upgrade.sh"; then
      install -m 0755 "$tmp/upgrade.sh" "$SCRIPT_PATH"
      note "升级脚本已保存到 $SCRIPT_PATH（后续升级: sudo nft-forward-upgrade）"
    else
      warn "下载的 install.sh 内容异常（疑似错误页/截断），已跳过保存升级脚本"
    fi
  fi
}

do_update_script() {
  local stmp surl
  surl="$(script_url)"
  stmp="$(mktemp -d)"
  note "下载最新 install.sh ..."
  curl -fsSL "$surl" -o "$stmp/upgrade.sh" \
    || { rm -rf "$stmp"; die "下载失败: $surl"; }
  if ! looks_like_installer "$stmp/upgrade.sh"; then
    rm -rf "$stmp"
    die "下载的 install.sh 内容异常（疑似错误页/截断），拒绝覆盖 $SCRIPT_PATH"
  fi
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

# 下载一个 release 资产并对照该 release 的 SHA256SUMS 强校验。成功时 stdout 只输出
# 已校验的 sha256 十六进制串（调用方把它捕获进身份文件），其余输出一律走 stderr。
#   $1 = base URL（releases/download 根）  $2 = 资产名  $3 = 落盘路径
#
# SHA256SUMS 在这里同时充当 release 的资产清单，所以先取它：
#   - 取不到 → 无从校验，拒绝继续；不存在“跳过校验直接装”的路径。
#   - 清单里没有 $2 → 在下载之前就报出原因。arm64 产物是后来才加入发布的，
#     --release 指向更早的版本时走到的就是这一支。
# 期望值按资产名从清单里取出，再与落盘文件的实际 sha256 比对；不用
# `sha256sum -c`，因为它按清单里写的文件名去找文件，而资产名（如
# nft-agent-arm64）与落盘用的安装名（nft-agent）并不相同。
fetch_and_verify() {
  local base="$1" asset="$2" dest="$3"
  local sums want got
  sums="$(curl -fsSL "$base/SHA256SUMS")" \
    || die "未取到 SHA256SUMS：必须强校验，拒绝裸跑（检查网络/代理或稍后重试）"
  want="$(awk -v a="$asset" '$2 == a { print $1 }' <<<"$sums")"
  [[ -n "$want" ]] \
    || die "SHA256SUMS 中没有 ${asset}：该 release 未发布当前架构（${arch}）的产物，或取到的清单不完整。arm64 产物只在较新的 release 中提供，可改用更新的 --release"
  curl -fL --progress-bar "$base/$asset" -o "$dest" \
    || die "下载失败: $base/$asset"
  got="$(sha256sum "$dest" | awk '{print $1}')"
  [[ "$got" == "$want" ]] \
    || die "sha256 校验失败: ${asset}（期望 ${want}，实际 ${got}）"
  echo "sha256 OK: $asset" >&2
  echo "$got"
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

  note "[1/5] 下载二进制 ($tag, $arch) ..."
  agent_sha="$(fetch_and_verify "$base" "$agent_asset" "$_update_tmp/nft-agent")"
  if [[ "$want_server" -eq 1 ]]; then
    fetch_and_verify "$base" "$server_asset" "$_update_tmp/nft-server" >/dev/null
  fi

  # 这里不对下载到的二进制再做架构探测，它的架构由三件事合起来保证：
  #   - 资产名在进入任何模式之前就由 uname -m 定下（见下方“架构 → release 资产名”）；
  #   - 上面的 sha256 把下载内容逐字节钉在 release 里以该名字发布的那份资产上；
  #   - “以该名字发布的资产确实是该架构的 ELF”由发版时 scripts/build-release.sh
  #     对每个产物的 ELF 头自检保证。
  # 二进制在本机是否真的能跑，由替换后的 health-check 证明：daemon 起不来会
  # 自动回滚到 .bak。

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

  # A server host's daemon unit must carry --server-local: that is what makes
  # the daemon connect to the co-located panel (through the panel's local
  # socket) and keep its "panel" segment across restarts instead of converting
  # it to local rules. do_update leaves units untouched otherwise — it lacks the
  # agent's --connect config — but the server daemon unit has no such
  # install-time args to lose.
  if [[ "$want_server" -eq 1 ]]; then
    write_daemon_unit " --server-local"
  fi

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
relay_host=""
relay_host_v6=""
addr=""
purge=0
RESET_PW=""
allow_insecure="${NFTF_ALLOW_INSECURE:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    tui|server|agent|update|update-script|uninstall|reset-password)
      if [[ -z "$mode" ]]; then
        mode="$1"
      elif [[ "$mode" == "uninstall" ]]; then
        UNINSTALL_TARGET="$1"
      else
        die "未知参数: $1"
      fi
      shift ;;
    daemon|all)
      if [[ "$mode" == "uninstall" ]]; then UNINSTALL_TARGET="$1"; shift; continue; fi
      die "未知参数: $1" ;;
    --panel-url) require_val --panel-url "${2:-}"; panel_url="$2"; shift 2 ;;
    --panel-url=*) panel_url="${1#*=}"; shift ;;
    --token) require_val --token "${2:-}"; token="$2"; shift 2 ;;
    --token=*) token="${1#*=}"; shift ;;
    --port-range) require_val --port-range "${2:-}"; port_range="$2"; shift 2 ;;
    --port-range=*) port_range="${1#*=}"; shift ;;
    --relay-host) require_val --relay-host "${2:-}"; relay_host="$2"; shift 2 ;;
    --relay-host=*) relay_host="${1#*=}"; shift ;;
    --relay-host-v6) require_val --relay-host-v6 "${2:-}"; relay_host_v6="$2"; shift 2 ;;
    --relay-host-v6=*) relay_host_v6="${1#*=}"; shift ;;
    --addr) require_val --addr "${2:-}"; addr="$2"; shift 2 ;;
    --addr=*) addr="${1#*=}"; shift ;;
    --release) require_val --release "${2:-}"; RELEASE="$2"; RELEASE_EXPLICIT=1; shift 2 ;;
    --release=*) RELEASE="${1#*=}"; RELEASE_EXPLICIT=1; shift ;;
    --gh-proxy) require_val --gh-proxy "${2:-}"; GH_PROXY="$2"; GH_PROXY_EXPLICIT=1; shift 2 ;;
    --gh-proxy=*) GH_PROXY="${1#*=}"; GH_PROXY_EXPLICIT=1; shift ;;
    --purge) purge=1; shift ;;
    --insecure) allow_insecure=1; shift ;;
    --password) require_val --password "${2:-}"; RESET_PW="$2"; shift 2 ;;
    --password=*) RESET_PW="${1#*=}"; shift ;;
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

# 架构 → release 资产名
# release 为每种架构各发布一套 nft-agent / nft-server。这里按 uname -m 定下本机要取
# 的资产名，此后的下载与 sha256 校验都只认这两个名字；装到 $INSTALL_DIR 时仍用不带
# 后缀的安装名，unit 文件与升级回滚逻辑因此与架构无关。
# amd64 沿用无后缀资产名：更早的 release 只发布过这一套，--release 指向它们时仍要
# 装得上；arm64 带 -arm64 后缀。较新的 release 里还有内容相同的 nft-agent-amd64 /
# nft-server-amd64，那是供按架构名手工取用的别名，这里不取。
# 同一口径还有两处落点——scripts/build-release.sh 的构建矩阵、面板推送升级用的
# internal/server/upgrade.go agentAssets——增删架构必须三处同改。
arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  asset_suffix="" ;;
  aarch64|arm64) asset_suffix="-arm64" ;;
  *) die "不支持的架构: ${arch}（release 仅提供 amd64 / arm64 二进制，可自行交叉编译）" ;;
esac
agent_asset="nft-agent$asset_suffix"
server_asset="nft-server$asset_suffix"

# Mode selection (interactive when no TTY arg).
if [[ -z "$mode" ]]; then
  [[ -t 0 ]] || die "未指定模式。管道用法: curl ... | bash -s <tui|server|agent>；详见 --help"
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
    if [[ -z "$addr" && -z "${PANEL_ADDR:-}" ]]; then
      # Preserve existing bind address on re-install.
      if [[ -f "$SYSTEMD_DIR/nft-forward-server.service" ]]; then
        addr="$(sed -n 's/^ExecStart=.*nft-server --addr //p' "$SYSTEMD_DIR/nft-forward-server.service" | head -1)"
      fi
      if [[ -z "$addr" && -t 0 ]]; then
        read -rp "面板绑定地址（端口或 地址:端口，默认 :7788）: " _port
        _port="${_port:-7788}"
        if [[ "$_port" == *:* ]]; then
          addr="$_port"
        else
          addr=":$_port"
        fi
      else
        addr="${addr:-:7788}"
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
  elif [[ "$RELEASE" == "latest" ]]; then
    base="${GH_PROXY}https://github.com/$REPO/releases/latest/download"
  else
    base="${GH_PROXY}https://github.com/$REPO/releases/download/$RELEASE"
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
note "[1/3] 下载 nft-agent ($RELEASE, $arch) ..."
agent_sha="$(fetch_and_verify "$base" "$agent_asset" "$tmp/nft-agent")"
if [[ "$mode" == "server" ]]; then
  note "      下载 nft-server ($RELEASE, $arch) ..."
  fetch_and_verify "$base" "$server_asset" "$tmp/nft-server" >/dev/null
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
    restart_daemon
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
    write_daemon_unit " --server-local"
    write_server_unit "$addr"
    restart_daemon
    # 面板与 daemon 是配对的：daemon 带着 --server-local 重启后去连面板的本机入口，
    # 所以面板也必须是刚装上的这一份在运行。重装时面板进程可能还是替换前的旧二进制，
    # 同样只有 restart 才换得掉。
    systemctl enable nft-forward-server.service
    systemctl restart nft-forward-server.service
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
      echo "如需重置: sudo nft-forward-upgrade reset-password"
    fi
    ;;

  agent)
    switch_role_cleanup agent
    # Normalize URL: https→wss, http→ws; append /v1/agents if path empty.
    # The control channel carries root-level upgrade frames, so plaintext
    # (http/ws) is refused unless the operator explicitly opts in with
    # --insecure / NFTF_ALLOW_INSECURE=1 (local testing only).
    panel_url="${panel_url:-${PANEL_URL:-}}"
    [[ -n "$panel_url" ]] || die "agent 模式需要 --panel-url 或 PANEL_URL"
    insecure_scheme=""
    case "$panel_url" in
      https://*) panel_url="wss://${panel_url#https://}" ;;
      wss://*) ;;
      http://*)  panel_url="ws://${panel_url#http://}";  insecure_scheme=1 ;;
      ws://*) insecure_scheme=1 ;;
      *) die "panel-url 必须以 http(s):// 或 ws(s):// 开头" ;;
    esac
    connect_insecure_arg=""
    if [[ -n "$insecure_scheme" ]]; then
      if [[ -n "$allow_insecure" ]]; then
        warn "使用明文控制信道 $panel_url —— 存在中间人注入升级帧实现 root RCE 的风险，仅限本地测试"
        connect_insecure_arg=" --insecure-connect"
      else
        die "拒绝明文控制信道：--panel-url 请使用 https:// (wss://)。控制信道会下发 root 级升级帧，明文可被中间人劫持实现 RCE；确为本地测试请加 --insecure（或 NFTF_ALLOW_INSECURE=1）"
      fi
    fi
    case "$panel_url" in
      *"/v1/agents"|*"/v1/agents/") ;;
      */) panel_url="${panel_url}v1/agents" ;;
      *)  panel_url="${panel_url}/v1/agents" ;;
    esac
    mkdir -p /etc/nft-forward
    install -m 0600 /dev/stdin /etc/nft-forward/panel.token <<<"$token"
    range_arg=""
    [[ -n "$port_range" ]] && range_arg=" --port-range $port_range"
    relay_arg=""
    [[ -n "$relay_host" ]] && relay_arg+=" --relay-host $relay_host"
    [[ -n "$relay_host_v6" ]] && relay_arg+=" --relay-host-v6 $relay_host_v6"
    write_daemon_unit " --connect $panel_url --panel-token-file /etc/nft-forward/panel.token${range_arg}${relay_arg}${connect_insecure_arg}"
    restart_daemon
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
