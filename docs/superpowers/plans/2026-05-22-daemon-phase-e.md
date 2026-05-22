# install.sh + README + dead-code cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **CRITICAL (CLAUDE.md hard rule):** Commit messages, code comments, KDoc, shell-script comments, and Markdown headings/text MUST NOT carry process metadata — no "Phase X", "Task N", "Step Y", "Round Z", scheme codenames, "per previous review", etc. Explain *why* (design intent, invariant) instead. When dispatching subagents, pass this rule into their prompt; if their output violates it, clean it up before merging.

**Goal:** Retire the pre-daemon-ization surface of the project. Remove the now-orphan `apply` subcommand, `internal/store`, and `internal/systemd` packages. Rewrite `install.sh` so a single `nft-forward` binary supports tui / server / agent on the same host with daemon-first installation and clean migration from the old multi-binary layout. Rewrite the docker fixture and `README.md` to match.

**Architecture:**
- The daemon is the only nftables controller and persists its own state — there is no longer a need for a "boot-time apply" path that reads `/etc/nft-forward/rules.json` directly. `nft-forward apply` and the `internal/store` package are deleted.
- `internal/systemd` had no callers after Phase D removed the install-service / uninstall-service handlers; it is deleted.
- `install.sh` becomes a single-binary installer with three role sub-commands (`tui`, `server`, `agent`) that stack: each role assumes the daemon is already installed. The script writes `nft-forward-daemon.service` (and, for the server role, `nft-forward-server.service`); the agent role patches the daemon unit's `ExecStart` to add `--listen` and a token file. The script detects and removes the legacy `nft-forward.service` / `nft-server.service` / `nft-agent.service` units and the legacy `/usr/local/sbin/{nft-agent,nft-server}` binaries.
- `docker/` becomes a single `Dockerfile` building the `nft-forward` binary, with `docker-compose.yml` standing up a daemon container + a server container; `test.sh` is adapted to drive the new architecture.
- `README.md` is rewritten end-to-end with the single-binary, daemon-first, role-stacked narrative.

**Tech Stack:** Bash (install.sh), Dockerfile, docker-compose, Markdown. Minimal Go changes (deletions only).

---

## File Structure

**Deleted:**
- `internal/systemd/` (whole package — no callers in the repo).
- `internal/store/` (whole package — only caller was the now-removed apply path).
- `docker/Dockerfile.agent` and `docker/Dockerfile.server` (superseded by a single Dockerfile).

**Created:**
- `docker/Dockerfile` (single image for `nft-forward`).

**Modified:**
- `cmd/nft-forward/main.go` — remove `apply` from the dispatcher and remove `runApplyCompat`; drop the `internal/store` and `internal/daemonclient` imports if they become dead in this file (daemonclient is still used by `runTUI`, so it stays).
- `install.sh` — full rewrite (see Task 4 for the layout).
- `docker/docker-compose.yml` — single image, two services (daemon + server) using the same image.
- `docker/test.sh` — adapt to the new compose file and the new commands.
- `README.md` — full rewrite (see Task 6 for the section outline).
- `docs/daemon-manual-verification.md` — update the "Known limits" paragraph to drop the install-and-startup-flow follow-up that this phase is closing.

---

## Sequencing rationale

Bottom-up again: delete code first (no risk of stranding callers), then `install.sh` (the operator-facing entrypoint), then `docker/` (CI-facing fixture), then `README.md` (the most subjective). Every task ends with a green `go build ./... && go test ./...`. The shell-script and Markdown tasks have no automated test gate, so each task includes a concrete observable check the implementer must perform (e.g., `bash -n install.sh` for shell syntax, `docker compose config` for compose validity).

---

### Task 1: drop the `apply` subcommand and the `internal/store` package

**Files:**
- Modify: `cmd/nft-forward/main.go`
- Delete: `internal/store/` (whole directory)

**Context:** The `apply` subcommand was the entry point of the legacy `nft-forward.service` systemd unit (`ExecStart=nft-forward --apply`, later `nft-forward apply`). It read `/etc/nft-forward/rules.json` through `internal/store.Load()` and posted the rules to the daemon. With the daemon now persisting its own state and starting from systemd, there is no second source-of-truth — the apply path is doing the same work the daemon already does at boot. Deleting it removes the last consumer of `internal/store`, so the package goes with it.

- [ ] **Step 1: Remove the dispatch and the function**

Open `cmd/nft-forward/main.go`. In `main()`, delete the `case "apply":` arm. Delete the `runApplyCompat` function entirely. After this edit, search for orphan imports — `internal/store` is the most likely casualty. `internal/daemonclient` is still used by `runTUI`, so leave it.

- [ ] **Step 2: Delete the package**

```
rm -rf internal/store
```

- [ ] **Step 3: Verify the deletion is clean**

```
grep -RIn 'internal/store' --include='*.go' .
```

Expected: no matches.

```
grep -RIn 'runApplyCompat\|"apply"' --include='*.go' .
```

Expected: no matches (the string `"apply"` may legitimately appear in tests for the `applier` interface — read each hit and confirm it isn't the old subcommand dispatch).

- [ ] **Step 4: Build + test**

```
go build ./...
go test ./... -count=1
go vet ./...
```

All must pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/nft-forward/main.go internal/store
git commit -m "drop the apply subcommand and the store package the daemon replaced"
```

---

### Task 2: delete the `internal/systemd` package

**Files:**
- Delete: `internal/systemd/` (whole directory)

**Context:** Phase D removed the only callers (`runInstallService`, `runUninstall`). The package itself was left in place by Phase D for review hygiene. With `install.sh` taking over the systemd-unit lifecycle entirely, the in-process service helper is dead weight.

- [ ] **Step 1: Confirm zero callers**

```
grep -RIn 'nft-forward/internal/systemd' --include='*.go' .
```

Expected: no matches. If any match exists, STOP and report BLOCKED — Phase D missed a caller and we need to understand why before deleting.

- [ ] **Step 2: Delete**

```
rm -rf internal/systemd
```

- [ ] **Step 3: Build + test**

```
go build ./...
go test ./... -count=1
go vet ./...
```

All must pass.

- [ ] **Step 4: Commit**

```bash
git add internal/systemd
git commit -m "drop the systemd package now that install.sh owns unit installation"
```

---

### Task 3: write the new systemd unit templates as embedded strings in `install.sh`

**Files:**
- Modify: `install.sh` (just the unit-template section — the rest is Task 4)

**Context:** Before rewriting the full `install.sh` flow it helps to lock in the exact unit-file shapes so subsequent install logic just templates them. The plan source for these unit shapes is the daemon design spec (`docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md` lines 165–199); copy them into install.sh as heredoc-producing functions.

This task is small and focused: it changes nothing the operator sees, only puts the right text in the right place ready for Task 4 to call.

- [ ] **Step 1: Add three writer functions**

Near the top of `install.sh` (after the existing constants like `INSTALL_DIR`), insert these three functions:

```bash
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
```

- [ ] **Step 2: Validate shell syntax**

```
bash -n install.sh
```

Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add install.sh
git commit -m "install: introduce unit-template helpers and legacy cleanup"
```

---

### Task 4: rewrite the rest of `install.sh` for single-binary, daemon-first, role-stacked

**Files:**
- Modify: `install.sh`

**Context:** This is the biggest task. Everything below the helpers from Task 3 gets rewritten. The shape after this task:

```
usage()                # --help banner (rewritten)
parse args             # mode in {tui,server,agent,uninstall}; --port, --token, --release, --addr
detect arch
ensure mode (or prompt)
detect / fetch release
download nft-forward binary; install to /usr/local/sbin
mode dispatch:
  tui    -> ensure daemon unit, enable, print "run sudo nft-forward"
  server -> ensure daemon unit, write server unit, enable both
  agent  -> ensure daemon unit with --listen :PORT --token-file ...,
            write token file, enable + restart
uninstall <role>: see Step 4
```

- [ ] **Step 1: Replace `usage()`**

The new help banner replaces `tui|server|agent` semantics with daemon-first language:

```bash
usage() {
  cat <<USAGE
nft-forward 一键安装脚本

用法:
  $0 [tui|server|agent|uninstall] [选项]

模式:
  tui              单机 TUI（host daemon 已被自动安装为 systemd 服务）
  server           控制面板（依赖 daemon；自动叠加安装）
  agent            受控节点（让 daemon 额外监听 HTTP；接受远程 panel 推送）
  uninstall <角色> 卸载指定角色（server / agent / daemon）；daemon 单独卸载前请先卸 server/agent

选项 / 环境变量:
  --port PORT      (PORT)          端口；server 默认 8080，agent 默认 7878
  --token TOKEN    (AGENT_TOKEN)   agent bearer token（agent 模式必填）
  --addr ADDR      (PANEL_ADDR)    server 监听地址；默认 :8080
  --release VER    (NFTF_RELEASE)  GitHub release tag，默认 latest
  -h, --help                       显示此帮助

示例:
  sudo $0                                # 交互式
  sudo $0 server --addr :9000            # 自定义面板端口
  sudo $0 agent --port 7900 --token abc  # 远程节点
  sudo $0 uninstall server               # 仅卸面板，保留 daemon
USAGE
}
```

- [ ] **Step 2: Rewrite the dispatcher body**

After the existing `[[ $EUID -eq 0 ]]` check + arch detection (which stay), replace everything from `# 模式选择` to the end of the script with the body below.

```bash
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
      ok "已卸载 server 角色（daemon 保留）"
      ;;
    agent)
      # Restore the daemon unit to a no-listen ExecStart and remove the token file.
      write_daemon_unit ""
      rm -f /etc/nft-forward/daemon.token
      systemctl daemon-reload
      systemctl restart nft-forward-daemon.service
      ok "已卸载 agent 角色（daemon 保留，去掉 --listen）"
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
      ok "已卸载 daemon（state.json 保留在 /var/lib/nft-forward/）"
      ;;
    *) die "未知卸载目标: $UNINSTALL_TARGET" ;;
  esac
  exit 0
fi

# All install modes need the binary + the daemon unit. Download once, install
# once, then layer the role-specific unit on top.
remove_legacy_units

if [[ "$RELEASE" == "latest" ]]; then
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
首次启动的 admin 密码: journalctl -u nft-forward-server.service | grep 密 \\
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
```

- [ ] **Step 3: Update the parameter parser to accept `--addr` and `uninstall`**

Above the original parser block, add `addr=""` next to `mode=""`, `port=""`, `token=""`. Add a `--addr <value>` arm to the `case "$1"` loop. Accept `uninstall` as a mode value alongside `tui|server|agent`. Recognise `UNINSTALL_TARGET` as the env-var equivalent for non-interactive `uninstall <role>`.

The parser becomes:

```bash
mode=""
port=""
token=""
addr=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    tui|server|agent|uninstall) mode="$1"; shift ;;
    --port) port="${2:?--port 需要值}"; shift 2 ;;
    --port=*) port="${1#*=}"; shift ;;
    --token) token="${2:?--token 需要值}"; shift 2 ;;
    --token=*) token="${1#*=}"; shift ;;
    --addr) addr="${2:?--addr 需要值}"; shift 2 ;;
    --addr=*) addr="${1#*=}"; shift ;;
    --release) RELEASE="${2:?--release 需要值}"; shift 2 ;;
    --release=*) RELEASE="${1#*=}"; shift ;;
    server|agent|daemon)
      if [[ "$mode" == "uninstall" ]]; then UNINSTALL_TARGET="$1"; shift; continue; fi
      die "未知参数: $1" ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done
```

The `server|agent|daemon` arm is only meaningful when `mode=uninstall` and is parsed as the uninstall target.

- [ ] **Step 4: Validate**

```
bash -n install.sh
shellcheck install.sh || true   # informational; install.sh has been written without shellcheck in CI
```

The `bash -n` must pass. If `shellcheck` is available, treat any "error" (not "warning") as a regression worth fixing.

Then do a manual dry-run review: read the script top-to-bottom yourself and confirm each mode's flow ends with the operator getting the right message.

- [ ] **Step 5: Commit**

```bash
git add install.sh
git commit -m "install: single-binary daemon-first installer with role-stacked layers"
```

---

### Task 5: rewrite `docker/` for the single-binary model

**Files:**
- Create: `docker/Dockerfile`
- Delete: `docker/Dockerfile.agent`, `docker/Dockerfile.server`
- Modify: `docker/docker-compose.yml`
- Modify: `docker/test.sh`

**Context:** The docker fixture today builds two separate images (agent, server) from the deprecated binaries. After this task it builds one image — the same `nft-forward` binary — and runs two containers from it: one as a daemon, one as a server, with the daemon's unix socket bind-mounted between them.

- [ ] **Step 1: Read what test.sh currently exercises**

```
cat docker/test.sh
```

It almost certainly drives the old `nft-server` HTTP API. Note what scenarios it asserts (login, add node, add forward, etc.) — you'll keep those scenarios, just talk to `nft-forward server` instead of `nft-server`.

- [ ] **Step 2: Write the single Dockerfile**

Create `docker/Dockerfile`:

```dockerfile
# Single image for nft-forward; the container's command decides the role.
# Build stage produces a static binary; runtime stage installs the
# minimal userland the daemon and panel need.
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nft-forward ./cmd/nft-forward

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends nftables iproute2 ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/nft-forward /usr/local/sbin/nft-forward
# No ENTRYPOINT — docker-compose.yml supplies the per-service command so the
# same image serves daemon, server, or agent roles.
```

- [ ] **Step 3: Delete the old Dockerfiles**

```
rm docker/Dockerfile.agent docker/Dockerfile.server
```

- [ ] **Step 4: Rewrite `docker/docker-compose.yml`**

```yaml
services:
  daemon:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    image: nft-forward:dev
    container_name: nftf-daemon
    cap_add: ["NET_ADMIN", "NET_RAW", "SYS_MODULE"]
    network_mode: host
    volumes:
      - daemon-state:/var/lib/nft-forward
      - daemon-run:/var/run
    command: ["/usr/local/sbin/nft-forward", "daemon"]

  server:
    image: nft-forward:dev
    depends_on: [daemon]
    container_name: nftf-server
    network_mode: host
    volumes:
      - daemon-run:/var/run             # share the daemon's unix socket
      - server-data:/var/lib/nft-forward
    command: ["/usr/local/sbin/nft-forward", "server", "--addr", ":8080"]

volumes:
  daemon-state:
  daemon-run:
  server-data:
```

(Both services use `network_mode: host` because the daemon needs to manage host nftables and the panel binds the operator-visible HTTP port. Shared `daemon-run` volume gives the server access to `/var/run/nft-forward.sock`.)

- [ ] **Step 5: Adapt `docker/test.sh`**

Replace any `docker compose build` + `docker compose run agent ...` / `docker compose run server ...` shape with `docker compose up -d daemon server` and then drive scenarios over `http://localhost:8080`. Concrete edits depend on what the file does today (read it first); preserve the assertions, change only the orchestration. Examples:

- Replace `docker compose exec server /usr/local/sbin/nft-server ...` with `docker compose exec server /usr/local/sbin/nft-forward server ...` (or just exercise the running container's HTTP API).
- Replace `docker compose exec agent ...` with `docker compose exec daemon /usr/local/sbin/nft-forward daemon --help` (smoke check) plus curl against the unix socket through `docker compose exec daemon curl --unix-socket /var/run/nft-forward.sock http://daemon/v1/health`.

If the existing script is heavily coupled to two binaries and adapting it cleanly would balloon scope, write a smaller replacement that asserts the new architecture's basic invariants:

1. `docker compose up -d` succeeds.
2. `curl http://localhost:8080/healthz` returns 200.
3. `docker compose exec daemon curl --unix-socket /var/run/nft-forward.sock http://daemon/v1/health` returns `{"ok":true}`.
4. `docker compose down -v` cleans up.

State which approach you took in the commit body.

- [ ] **Step 6: Validate**

```
docker compose --file docker/docker-compose.yml config           # YAML / schema sane
bash -n docker/test.sh
```

The `compose config` parse must succeed. (Don't actually build the image — it pulls golang base layers and takes minutes.)

- [ ] **Step 7: Commit**

```bash
git add docker/
git commit -m "docker: single image for the host daemon and the panel"
```

---

### Task 6: rewrite `README.md`

**Files:**
- Modify: `README.md`

**Context:** The current README describes three binaries, three install paths, and three systemd units. It needs to be rewritten end-to-end so a new reader's mental model is: one binary, one daemon, layer roles on top. The architecture spec (`docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md`) is the source of truth — when you write the architecture section, match its terminology exactly so the two documents don't drift.

This is a content task with no test gate; you must read the existing README, understand its sections, and produce a coherent replacement.

- [ ] **Step 1: Read the current README cover-to-cover**

```
cat README.md
```

Note which sections describe binary-specific surface that needs to go (anything mentioning `nft-server`, `nft-agent` as binaries, or `nft-forward.service` as a unit).

- [ ] **Step 2: Write the new README**

The new structure (adjust headings to match the project's existing language — Chinese, per the current file):

```
# nft-forward

## 是什么
（一句话定位 + 关键卖点：单二进制 host daemon、所有规则归 daemon 管、TUI/server/agent 角色叠加）

## 快速开始
- TUI（单机）：sudo bash install.sh
- 控制面板：sudo bash install.sh server
- 远程节点：sudo bash install.sh agent --token <从面板拷贝>

## 架构（一图 + 200 字）
- nft-forward daemon：唯一调用 nftables / tc 的进程，监听 /var/run/nft-forward.sock；可选 --listen :7878 接受 HTTP 远程推送。
- TUI / server 都是 daemon 的 client；server 通过 daemon 推送规则到所有节点。
- owner-segmented ruleset：tui / panel 共存，按端口冲突，daemon 合并后下发。

## 命令表面
- nft-forward                  默认进 TUI（要求 daemon 已运行）
- nft-forward daemon           前台启动 daemon（systemd 通常负责）
- nft-forward daemon --listen :7878 --token-file <path>  agent 角色
- nft-forward server [--addr :8080] [--db <path>]        web 面板

## 配置 / 持久化
- daemon state: /var/lib/nft-forward/state.json
- daemon socket: /var/run/nft-forward.sock (group nft-forward, mode 0660)
- agent token: /etc/nft-forward/daemon.token (mode 0600)
- panel DB: /var/lib/nft-forward/panel.db

## 升级 / 迁移
- install.sh 自动 disable + 删除旧的 nft-forward.service / nft-server.service / nft-agent.service
- /usr/local/sbin/{nft-agent,nft-server} 旧二进制被 install.sh 自动清理
- daemon 首次启动会把旧 /etc/nft-forward/rules.json / /var/lib/nft-forward/agent-state.json 自动迁入新 state.json 并把原文件重命名为 .migrated

## 开发
- Go 1.22+，纯 stdlib（除 charmbracelet TUI 库）
- 跑测试：go test ./...
- docker dev fixture：docker compose --file docker/docker-compose.yml up

## 协议参考
- POST /v1/ruleset/{owner}  全量替换 owner segment
- GET  /v1/ruleset           分段返回当前 owner -> rules
- GET  /v1/counters          每条规则的字节/包计数
- GET  /v1/health            探活
- 详细 spec：docs/superpowers/specs/2026-05-21-single-binary-daemon-design.md
```

Write enough prose under each heading that the reader doesn't need to chase external links for the basics, but don't reproduce the design spec. Aim for 2-3 short paragraphs per top-level section.

**Do not include any "Phase X" or task-number language.** The README describes the project's *current* shape, not how it got there.

- [ ] **Step 3: Update `docs/daemon-manual-verification.md`**

Find the existing "Known limits" or equivalent section. Remove the bullet that pointed at install.sh + nft-forward.service refactor as "next phase" — that bullet is now obsolete. Replace it with something like:

> 当前所有自动化测试都在 `internal/daemon` 与 `internal/daemonclient` 单元层级覆盖；端到端 systemd / install.sh 流程仍依赖手工验证。

- [ ] **Step 4: Visual / link sanity check**

```
grep -n 'nft-agent\|nft-server' README.md
```

After the rewrite, surviving hits should only be in *migration* or *uninstall* context (telling the reader that those binaries used to exist), not as installation guidance.

```
grep -n 'nft-forward\.service' README.md
```

Same — surviving hits should be in the "you may see this on upgrade" migration paragraph only, not as a unit to install.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/daemon-manual-verification.md
git commit -m "docs: rewrite the project landing for the single-binary daemon model"
```

---

## Self-review pass

After all six tasks land:

1. `grep -RIn "nft-forward/internal/store\|nft-forward/internal/systemd" --include='*.go' .` returns nothing.
2. `grep -RIn "runApplyCompat\|--apply\b" --include='*.go' .` returns nothing.
3. `grep -RIn 'Phase\|Task #\|Step [0-9]\|Round [0-9]' $(git log --name-only --pretty=format: <pre-phase-head>..HEAD | sort -u | grep -E '\.(go|sh|md|yml|Dockerfile)$')` returns nothing — code, scripts, docs, compose files must be free of process metadata.
4. `git log <pre-phase-head>..HEAD --format=%B | grep -iE 'phase|task #|step [0-9]|round [0-9]'` returns nothing — commit messages must be free of process metadata.
5. `go test ./... -count=1` is green.
6. `go vet ./...` is clean.
7. `bash -n install.sh` is clean.
8. `docker compose --file docker/docker-compose.yml config` parses without error.

If any check fails: fix and create a *successor* commit (never amend) per CLAUDE.md.

---

## Out of scope

- Multi-arch release artifacts (arm64, riscv64). `install.sh` still gates on amd64 only.
- TLS / mTLS for the daemon's HTTP listener.
- Anything from the design spec's "Out-of-scope" list at end of file.
- A real CI pipeline for the docker fixture — `test.sh` remains operator-driven.
