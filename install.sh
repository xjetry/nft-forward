#!/usr/bin/env bash
# nft-forward 一键安装脚本
# 从 GitHub release 下载 nft-forward 二进制并配置 host daemon / server / agent / uninstall
# 用法：见 --help

set -euo pipefail

REPO="xjetry/nft-forward"
RELEASE="${NFTF_RELEASE:-latest}"
INSTALL_DIR="/usr/local/sbin"
SYSTEMD_DIR="/etc/systemd/system"
_update_tmp=""

write_daemon_unit() {
  local extra_args="${1:-}"
  cat >"$SYSTEMD_DIR/nft-forward-daemon.service" <<EOF
[Unit]
Description=nft-forward host daemon (nftables controller)
After=network-online.target nftables.service
Wants=network-online.target

[Service]
ExecStart=$INSTALL_DIR/nft-forward daemon$extra_args
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
  local addr="${1:-:8080}"
  cat >"$SYSTEMD_DIR/nft-forward-server.service" <<EOF
[Unit]
Description=nft-forward web panel
Requires=nft-forward-daemon.service
After=nft-forward-daemon.service

[Service]
ExecStart=$INSTALL_DIR/nft-forward server --addr $addr
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
    if systemctl list-unit-files --no-legend | grep -q "^$unit "; then
      systemctl disable --now "$unit" 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/$unit"
    fi
  done

  # Earlier releases also dropped two stand-alone binaries that the daemon
  # has since absorbed. Remove them so PATH doesn't shadow nft-forward.
  for bin in nft-agent nft-server; do
    rm -f "$INSTALL_DIR/$bin"
  done
}

usage() {
  cat <<USAGE
nft-forward 一键安装/卸载/升级脚本

用法:
  $0 [tui|server|agent|update|uninstall] [选项]

模式:
  tui              单机 TUI（host daemon 已被自动安装为 systemd 服务）
  server           控制面板（依赖 daemon；自动叠加安装）
  agent            受控节点（让 daemon 额外监听 HTTP；接受远程 panel 推送）
  update           拉 latest 二进制原子替换 + restart + 失败回滚
  uninstall <角色> 卸载指定角色（server / agent / daemon）；daemon 单独卸载前请先卸 server/agent

选项 / 环境变量:
  --port PORT      (PORT)          agent 监听端口；默认 7878
  --token TOKEN    (AGENT_TOKEN)   agent bearer token（agent 模式必填）
  --addr ADDR      (PANEL_ADDR)    server 监听地址；默认 :8080
  --release VER    (NFTF_RELEASE)  GitHub release tag，默认 latest（update 模式禁用）
  --purge                          uninstall 模式专用：按角色 scope 清残留数据
  -h, --help                       显示此帮助

示例:
  sudo $0                                # 交互式
  sudo $0 server --addr :9000            # 自定义面板端口
  sudo $0 agent --port 7900 --token abc  # 远程节点
  sudo $0 update                         # 拉 latest 二进制升级
  sudo $0 uninstall server               # 仅卸面板，保留 daemon
  sudo $0 uninstall daemon --purge       # 完整擦除 daemon 残留
USAGE
}

die() { echo "错误: $*" >&2; exit 1; }
note() { printf '\033[36m%s\033[0m\n' "$*"; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }

rollback_update() {
  echo "update 失败，回滚到旧二进制" >&2
  if [[ -f "$INSTALL_DIR/nft-forward.bak" ]]; then
    mv -f "$INSTALL_DIR/nft-forward.bak" "$INSTALL_DIR/nft-forward"
  fi
  systemctl restart nft-forward-daemon.service 2>/dev/null || true
  if systemctl list-unit-files --no-legend \
     | grep -q '^nft-forward-server\.service '; then
    systemctl restart nft-forward-server.service 2>/dev/null || true
  fi
  exit 1
}

do_update() {
  # ---- 前置探测 ----
  [[ -x "$INSTALL_DIR/nft-forward" ]] \
    || die "未安装：$INSTALL_DIR/nft-forward 不存在；请先 install.sh tui/server/agent"
  systemctl list-unit-files --no-legend | grep -q '^nft-forward-daemon\.service ' \
    || die "未安装：nft-forward-daemon.service 不存在；请先 install.sh tui/server/agent"

  # ---- 下载到 tmp ----
  _update_tmp="$(mktemp -d)"
  trap 'rm -rf "$_update_tmp"' EXIT

  note "[1/5] 下载 nft-forward (latest) ..."
  curl -fL --progress-bar "$base/nft-forward" -o "$_update_tmp/nft-forward" \
    || die "下载失败: $base/nft-forward"

  note "[2/5] 校验 sha256 ..."
  if curl -fLs "$base/SHA256SUMS" -o "$_update_tmp/SHA256SUMS" 2>/dev/null; then
    (cd "$_update_tmp" && grep -E '  nft-forward$' SHA256SUMS | sha256sum -c -) \
      || die "sha256 校验失败"
  else
    die "未取到 SHA256SUMS：update 必须强校验，拒绝裸跑（检查网络或稍后重试）"
  fi

  # ---- 架构防护（产物侧）----
  file "$_update_tmp/nft-forward" | grep -q 'ELF 64-bit LSB.*x86-64' \
    || die "下载到的二进制不是 ELF 64-bit x86-64（content: $(file "$_update_tmp/nft-forward"))"

  # ---- exec 自检 ----
  local exec_rc=0
  "$_update_tmp/nft-forward" --version >/dev/null 2>&1 || exec_rc=$?
  if [[ $exec_rc -gt 125 ]]; then
    die "新二进制无法执行（exec format / 缺权限，退出码 $exec_rc）"
  fi

  # ---- 备份旧二进制 ----
  note "[3/5] 备份旧二进制到 $INSTALL_DIR/nft-forward.bak ..."
  cp -a "$INSTALL_DIR/nft-forward" "$INSTALL_DIR/nft-forward.bak"
  trap 'rm -rf "$_update_tmp"; rollback_update' ERR INT TERM

  # ---- 原子替换 ----
  install -m 0755 "$_update_tmp/nft-forward" "$INSTALL_DIR/nft-forward"

  # ---- 重启 unit ----
  note "[4/5] 重启 daemon (+ server, if present) ..."
  systemctl daemon-reload
  systemctl restart nft-forward-daemon.service
  if systemctl list-unit-files --no-legend \
     | grep -q '^nft-forward-server\.service '; then
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
  rm -f "$INSTALL_DIR/nft-forward.bak"
  local sha size
  sha=$(sha256sum "$INSTALL_DIR/nft-forward" | awk '{print $1}')
  size=$(stat -c %s "$INSTALL_DIR/nft-forward" 2>/dev/null || stat -f %z "$INSTALL_DIR/nft-forward")
  ok "===== Update 完成 ====="
  echo "二进制 sha256: $sha"
  echo "二进制 size:   $size 字节"
  echo "建议查看启动日志: journalctl -u nft-forward-daemon.service --since '1 minute ago'"
}

# 参数解析（--help 不需要 root）
mode=""
port=""
token=""
addr=""
purge=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    tui|server|agent|update|uninstall) mode="$1"; shift ;;
    --port) port="${2:?--port 需要值}"; shift 2 ;;
    --port=*) port="${1#*=}"; shift ;;
    --token) token="${2:?--token 需要值}"; shift 2 ;;
    --token=*) token="${1#*=}"; shift ;;
    --addr) addr="${2:?--addr 需要值}"; shift 2 ;;
    --addr=*) addr="${1#*=}"; shift ;;
    --release) RELEASE="${2:?--release 需要值}"; RELEASE_EXPLICIT=1; shift 2 ;;
    --release=*) RELEASE="${1#*=}"; RELEASE_EXPLICIT=1; shift ;;
    --purge) purge=1; shift ;;
    server|agent|daemon)
      if [[ "$mode" == "uninstall" ]]; then UNINSTALL_TARGET="$1"; shift; continue; fi
      die "未知参数: $1" ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

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
  echo "  1) tui        单机 TUI（自动装 daemon）"
  echo "  2) server     控制面板（叠加 daemon）"
  echo "  3) agent      远程节点（让 daemon 额外开 HTTP）"
  echo "  4) uninstall  卸载（再问要卸哪个角色）"
  read -rp "输入数字或名称: " choice
  case "$choice" in
    1|tui)       mode=tui ;;
    2|server)    mode=server ;;
    3|agent)     mode=agent ;;
    4|uninstall) mode=uninstall ;;
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
    port="${port:-${PORT:-7878}}"
    token="${token:-${AGENT_TOKEN:-}}"
    if [[ -z "$token" && -t 0 ]]; then
      read -rp "Agent bearer token（从面板节点详情页拷贝）: " token
    fi
    [[ -n "$token" ]] || die "agent 模式需要 --token 或 AGENT_TOKEN"
    ;;
  server)
    addr="${addr:-${PANEL_ADDR:-:8080}}"
    ;;
  uninstall)
    if [[ -z "${UNINSTALL_TARGET:-}" && -t 0 ]]; then
      read -rp "卸载哪个角色 [server/agent/daemon]: " UNINSTALL_TARGET
    fi
    UNINSTALL_TARGET="${UNINSTALL_TARGET:-}"
    [[ -n "$UNINSTALL_TARGET" ]] || die "uninstall 需要指定角色"
    ;;
esac

# Uninstall takes a separate code path (no download needed).
if [[ "$mode" == "uninstall" ]]; then
  case "$UNINSTALL_TARGET" in
    server)
      systemctl disable --now nft-forward-server.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-server.service"
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
        # POST empty panel segment first so daemon persists the clear into
        # state.json before we restart it under the new unit.
        curl -sf --unix-socket /var/run/nft-forward.sock \
             -X POST -H 'Content-Type: application/json' \
             http://daemon/v1/ruleset/panel \
             -d '{"rules":[]}' >/dev/null 2>&1 \
          || echo "警告: 未能通过 daemon API 清 panel 段（daemon 可能已停）" >&2
      fi
      # Restore the daemon unit to a no-listen ExecStart and remove the token file.
      write_daemon_unit ""
      rm -f /etc/nft-forward/daemon.token
      systemctl daemon-reload
      systemctl restart nft-forward-daemon.service
      if [[ "$purge" -eq 1 ]]; then
        rm -rf /etc/nft-forward/
        ok "已卸载 agent 角色 + 清 /etc/nft-forward/ 与 daemon panel 段"
      else
        ok "已卸载 agent 角色（daemon 保留，去掉 --listen；token 文件已删，panel 段保留）"
      fi
      ;;
    daemon)
      if systemctl is-active --quiet nft-forward-server.service \
         || systemctl list-unit-files --no-legend \
            | grep -qE '^nft-forward-server\.service '; then
        die "请先卸载 server 角色：sudo $0 uninstall server"
      fi
      systemctl disable --now nft-forward-daemon.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-daemon.service"
      rm -f "$INSTALL_DIR/nft-forward"
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
    *) die "未知卸载目标: $UNINSTALL_TARGET" ;;
  esac
  exit 0
fi

# Update is its own code path: no role unit changes, only binary swap.
if [[ "$mode" == "update" ]]; then
  if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
    base="$NFTF_RELEASE_BASE_URL"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  do_update
  exit 0
fi

# All install modes need the binary + the daemon unit. Download once, install
# once, then layer the role-specific unit on top.
remove_legacy_units

if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
  base="$NFTF_RELEASE_BASE_URL"
elif [[ "$RELEASE" == "latest" ]]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$RELEASE"
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

note "[1/3] 下载 nft-forward ($RELEASE) ..."
curl -fL --progress-bar "$base/nft-forward" -o "$tmp/nft-forward" \
  || die "下载失败: $base/nft-forward"

note "[2/3] 校验 sha256 ..."
if curl -fLs "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
  (cd "$tmp" && grep -E '  nft-forward$' SHA256SUMS | sha256sum -c -) \
    || die "sha256 校验失败"
else
  echo "    (SHA256SUMS 不可用，跳过校验)"
fi

note "[3/3] 安装到 $INSTALL_DIR/nft-forward ..."
install -m 0755 "$tmp/nft-forward" "$INSTALL_DIR/nft-forward"

primary_ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
primary_ip="${primary_ip:-<本机IP>}"

case "$mode" in
  tui)
    write_daemon_unit ""
    systemctl daemon-reload
    systemctl enable --now nft-forward-daemon.service
    cat <<EOF

$(ok "===== TUI 安装完成 =====")
host daemon 已作为 systemd 服务启动：nft-forward-daemon.service
运行 TUI:   sudo $INSTALL_DIR/nft-forward
TUI 会自动连接到上面的 daemon；规则在 daemon 重启后自动恢复。

文档:  https://github.com/$REPO#readme
EOF
    ;;

  server)
    write_daemon_unit ""
    write_server_unit "$addr"
    systemctl daemon-reload
    systemctl enable --now nft-forward-daemon.service
    systemctl enable --now nft-forward-server.service
    sleep 2
    cat <<EOF

$(ok "===== Server 安装完成 =====")
面板:        http://$primary_ip$addr
daemon unit: nft-forward-daemon.service
server unit: nft-forward-server.service
首次启动的 admin 密码: journalctl -u nft-forward-server.service | grep 密 \
  （或查看 server 启动日志）
EOF
    ;;

  agent)
    mkdir -p /etc/nft-forward
    install -m 0600 /dev/stdin /etc/nft-forward/daemon.token <<<"$token"
    write_daemon_unit " --listen :$port --token-file /etc/nft-forward/daemon.token"
    systemctl daemon-reload
    systemctl enable --now nft-forward-daemon.service
    cat <<EOF

$(ok "===== Agent 安装完成 =====")
daemon 现在同时监听 unix socket 与 :$port (HTTP, bearer auth)
在远端 panel 注册节点:
  地址: http://$primary_ip:$port
  Secret: $token

文档:  https://github.com/$REPO#readme
EOF
    ;;

  *) die "内部错误: 未处理的模式 $mode" ;;
esac
