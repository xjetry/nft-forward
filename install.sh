#!/usr/bin/env bash
# nft-forward 一键安装脚本
# 从 GitHub release 下载二进制并配置 systemd（server / agent 模式）
# 用法：见 --help

set -euo pipefail

REPO="xjetry/nft-forward"
RELEASE="${NFTF_RELEASE:-latest}"
INSTALL_DIR="/usr/local/sbin"
SYSTEMD_DIR="/etc/systemd/system"

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
nft-forward 一键安装脚本

用法:
  $0 [tui|server|agent] [选项]

模式:
  tui      单机 TUI（直接编辑本机 nftables）
  server   控制面板 + 内嵌 agent
  agent    受控节点（接收 panel 推送）

选项 / 环境变量:
  --port PORT          (PORT)         端口；server 默认 8080，agent 默认 7878
  --token TOKEN        (AGENT_TOKEN)  agent bearer token（agent 模式必填）
  --release VER        (NFTF_RELEASE) GitHub release tag，默认 latest
  -h, --help                          显示此帮助

示例:
  sudo $0                                   # 交互式
  sudo $0 server                            # server 走默认端口
  sudo $0 server --port 9000                # 自定义面板端口
  sudo $0 agent --port 7900 --token abc...  # 节点端口和 token

  # 一行 curl 安装 (无 TTY)
  curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh \\
    | sudo bash -s -- agent --port 7878 --token <从面板拷贝的 token>
USAGE
}

die() { echo "错误: $*" >&2; exit 1; }
note() { printf '\033[36m%s\033[0m\n' "$*"; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }

# 参数解析（--help 不需要 root）
mode=""
port=""
token=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    tui|server|agent) mode="$1"; shift ;;
    --port) port="${2:?--port 需要值}"; shift 2 ;;
    --port=*) port="${1#*=}"; shift ;;
    --token) token="${2:?--token 需要值}"; shift 2 ;;
    --token=*) token="${1#*=}"; shift ;;
    --release) RELEASE="${2:?--release 需要值}"; shift 2 ;;
    --release=*) RELEASE="${1#*=}"; shift ;;
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

# 模式选择
if [[ -z "$mode" ]]; then
  [[ -t 0 ]] || die "未指定模式且无 TTY；--help 查看用法"
  echo "请选择安装模式:"
  echo "  1) tui      单机 TUI"
  echo "  2) server   控制面板（含内嵌 agent）"
  echo "  3) agent    受控节点"
  read -rp "输入数字或名称 [1/2/3 或 tui/server/agent]: " choice
  case "$choice" in
    1|tui)    mode=tui ;;
    2|server) mode=server ;;
    3|agent)  mode=agent ;;
    *) die "未知选项: $choice" ;;
  esac
fi

# 端口（server / agent 模式）
case "$mode" in
  server) default_port=8080 ;;
  agent)  default_port=7878 ;;
  tui)    default_port="" ;;
esac

port="${port:-${PORT:-}}"
if [[ -z "$port" && -n "$default_port" ]]; then
  if [[ -t 0 ]]; then
    read -rp "$mode 端口 [$default_port]: " port
  fi
  port="${port:-$default_port}"
fi

# Token（仅 agent 模式）
if [[ "$mode" == "agent" ]]; then
  token="${token:-${AGENT_TOKEN:-}}"
  if [[ -z "$token" && -t 0 ]]; then
    read -rp "Agent bearer token（从面板节点详情页拷贝）: " token
  fi
  [[ -n "$token" ]] || die "agent 模式需要 --token 或 AGENT_TOKEN"
fi

# 决定二进制名
case "$mode" in
  tui)    binary="nft-forward" ;;
  server) binary="nft-server" ;;
  agent)  binary="nft-agent" ;;
esac

# 下载 URL
if [[ "$RELEASE" == "latest" ]]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$RELEASE"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

note "[1/4] 下载 $binary ($RELEASE) ..."
curl -fL --progress-bar "$base/$binary" -o "$tmp/$binary" || die "下载失败: $base/$binary"

note "[2/4] 校验 sha256 ..."
if curl -fLs "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
  (cd "$tmp" && grep -E "  $binary$" SHA256SUMS | sha256sum -c -) || die "sha256 校验失败"
else
  echo "    (SHA256SUMS 不可用，跳过校验)"
fi

note "[3/4] 安装到 $INSTALL_DIR/$binary ..."
install -m 0755 "$tmp/$binary" "$INSTALL_DIR/$binary"

primary_ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
primary_ip="${primary_ip:-<本机IP>}"

case "$mode" in
  tui)
    note "[4/4] 完成"
    cat <<EOF

$(ok "===== TUI 安装完成 =====")
运行:  sudo $INSTALL_DIR/$binary
首次启动会询问是否启用开机持久化（推荐选 Y），同意后自动安装 systemd 单元
nft-forward.service，规则在重启后自动恢复。

文档:  https://github.com/$REPO#readme
EOF
    ;;

  server)
    cat >"$SYSTEMD_DIR/nft-server.service" <<EOF
[Unit]
Description=nft-forward panel + embedded agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$INSTALL_DIR/nft-server --addr :$port
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now nft-server.service

    note "[4/4] 服务启动中 ..."
    sleep 2

    cat <<EOF

$(ok "===== Server 安装完成 =====")
面板地址:  http://${primary_ip}:${port}
systemd:   nft-server.service
状态:      systemctl status nft-server
日志:      journalctl -u nft-server -e
配置文件:  $SYSTEMD_DIR/nft-server.service

首次启动的随机 admin 密码已写入日志，运行以下命令查看:
  sudo journalctl -u nft-server -n 30 --no-pager | grep -A1 '密  码'

下一步:
  1. 浏览器打开面板，用 admin + 上述密码登录
  2. 右上「修改密码」改成自己的
  3. 在「节点」页添加远程节点，复制安装命令到目标机器执行
     （或者本机也已经作为 "localhost" 节点自动注册了）

文档:  https://github.com/$REPO#readme
EOF
    ;;

  agent)
    install -d /etc/nft-forward /var/lib/nft-forward
    echo "$token" > /etc/nft-forward/agent.token
    chmod 600 /etc/nft-forward/agent.token

    cat >"$SYSTEMD_DIR/nft-agent.service" <<EOF
[Unit]
Description=nft-forward agent
After=network-online.target nftables.service
Wants=network-online.target

[Service]
ExecStart=$INSTALL_DIR/nft-agent --listen :$port --token-file /etc/nft-forward/agent.token
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now nft-agent.service

    note "[4/4] 服务启动中 ..."
    sleep 2

    cat <<EOF

$(ok "===== Agent 安装完成 =====")
监听地址:  ${primary_ip}:${port}
systemd:   nft-agent.service
状态:      systemctl status nft-agent
日志:      journalctl -u nft-agent -e
Token:     /etc/nft-forward/agent.token

下一步: 回到面板「节点详情页」，点「重新同步」，状态应转为「已同步」。

如果一直「错误」:
  - 防火墙/安全组是否放行入站 ${port}/tcp
  - panel 是否能 reach 本机 ${primary_ip}:${port}
  - token 是否与面板上节点详情页一致:
      cat /etc/nft-forward/agent.token

文档:  https://github.com/$REPO#readme
EOF
    ;;
esac
