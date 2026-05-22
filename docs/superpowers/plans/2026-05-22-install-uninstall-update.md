# install / uninstall / update 完整化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `install.sh` 在已有 `install` / `uninstall <角色>` 之外覆盖 `update` 子命令、`uninstall --purge` 强清开关、角色切换自动清旧三处能力，闭环单一 bootstrap 入口。

**Architecture:** 完全在 `install.sh`（shell）内完成，不动 Go 代码。`docker/test.sh` 用新增的 `NFTF_RELEASE_BASE_URL` 环境变量绕过外网，端到端验证 update 成功/回滚与 `--purge` 清理范围。

**Tech Stack:** Bash + systemd + curl + sha256sum + nft + docker compose（既有 dev fixture）

**Spec:** `docs/superpowers/specs/2026-05-22-install-uninstall-update-design.md`

---

## File Structure

| 文件 | 职责 | 改动类型 |
|---|---|---|
| `install.sh` | 唯一可执行入口；承载 update / --purge / 角色切换全部新逻辑 | Modify |
| `docker/test.sh` | CI 冒烟测试；追加 update 成功/回滚、purge 三角色断言 | Modify |
| `docs/daemon-manual-verification.md` | 人工兜底验证；追加《install / update / uninstall 验证》章节 | Modify |
| `README.md` | 升级章节提示 `install.sh update` + 角色切换不动 state 的不变量 | Modify |

不新建任何文件。`install.sh` 体量从 ~293 行涨到 ~500 行左右仍单文件可维护。

---

### Task 1: NFTF_RELEASE_BASE_URL 环境变量

**目的：** 给 install / update 的下载逻辑加一个测试钩子，让 `docker/test.sh` 可以指向容器内本地 HTTP server，避免 CI 跑外网。

**Files:**
- Modify: `install.sh:214-218`

- [ ] **Step 1: 改造 base URL 构造**

在 `install.sh:214-218` 把：

```bash
if [[ "$RELEASE" == "latest" ]]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$RELEASE"
fi
```

替换成：

```bash
if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
  base="$NFTF_RELEASE_BASE_URL"
elif [[ "$RELEASE" == "latest" ]]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$RELEASE"
fi
```

- [ ] **Step 2: 本地手动验证 install 路径未受影响**

```bash
bash -n install.sh
```

期望：无输出（语法 OK）。env var 未设时旧行为完全保留——因为 `${NFTF_RELEASE_BASE_URL:-}` 在未设时展开为空串，进入 `elif`。

- [ ] **Step 3: Commit**

```bash
git add install.sh
git commit -m "install: introduce NFTF_RELEASE_BASE_URL override for test fixtures"
```

---

### Task 2: update 子命令完整实现

**目的：** 落地 spec §设计 → update 实现九步流程：前置探测 → 架构防护 → 下载 + 校验 → 备份 + trap → 原子替换 → restart → health-check → rollback / 成功收尾。

**Files:**
- Modify: `install.sh`（usage、参数解析、新增 `do_update()` 函数、新增 `rollback_update()` 函数、dispatch 入口）

- [ ] **Step 1: 更新 usage()**

把 `install.sh:71-97` 的 `usage()` 函数体替换为（保持现有所有行，仅在"模式"块加 `update`，"示例"块加 update 示例）：

```bash
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
```

- [ ] **Step 2: 参数解析加 update 模式 + --purge flag**

把 `install.sh:108-125` 的 `while` 循环里的 mode case：

```bash
tui|server|agent|uninstall) mode="$1"; shift ;;
```

替换为：

```bash
tui|server|agent|update|uninstall) mode="$1"; shift ;;
```

在 `--release=*) RELEASE="${1#*=}"; shift ;;` 之后加入 `--purge` 解析：

```bash
    --purge) purge=1; shift ;;
```

在循环上方变量初始化区（`addr=""` 之后）加：

```bash
purge=0
```

- [ ] **Step 3: 加 update 模式 die guards**

在 `install.sh:155` 的 `case "$mode" in` 块前加入（也就是 mode 选择之后、case 派发之前）：

```bash
if [[ "$mode" == "update" && -n "${RELEASE_EXPLICIT:-}" ]]; then
  die "update 只拉 latest，要锁版本请用 install（如 sudo $0 tui --release v1.2.3）"
fi
if [[ "$mode" != "uninstall" && "$purge" -eq 1 ]]; then
  die "--purge 仅 uninstall 模式有效"
fi
```

为了让 `RELEASE_EXPLICIT` 有意义，调整 `--release` 解析分支让它在被显式传入时设置标志。把：

```bash
--release) RELEASE="${2:?--release 需要值}"; shift 2 ;;
--release=*) RELEASE="${1#*=}"; shift ;;
```

改为：

```bash
--release) RELEASE="${2:?--release 需要值}"; RELEASE_EXPLICIT=1; shift 2 ;;
--release=*) RELEASE="${1#*=}"; RELEASE_EXPLICIT=1; shift ;;
```

- [ ] **Step 4: 写 rollback_update() 函数**

在 `ok() { ... }`（`install.sh:101`）之后插入：

```bash
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
```

放在这里的原因：`die`/`note`/`ok` 是已有的 helper 群，`rollback_update` 同属"全局工具函数"，放一起方便定位。

- [ ] **Step 5: 写 do_update() 主流程**

在 `rollback_update()` 之后插入：

```bash
do_update() {
  # ---- 前置探测 ----
  [[ -x "$INSTALL_DIR/nft-forward" ]] \
    || die "未安装：$INSTALL_DIR/nft-forward 不存在；请先 install.sh tui/server/agent"
  systemctl list-unit-files --no-legend | grep -q '^nft-forward-daemon\.service ' \
    || die "未安装：nft-forward-daemon.service 不存在；请先 install.sh tui/server/agent"

  # ---- 架构防护（本机侧）----
  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) ;;
    *) die "目前仅 amd64 二进制可用（当前: $arch）" ;;
  esac

  # ---- 下载到 tmp ----
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  note "[1/5] 下载 nft-forward (latest) ..."
  curl -fL --progress-bar "$base/nft-forward" -o "$tmp/nft-forward" \
    || die "下载失败: $base/nft-forward"

  note "[2/5] 校验 sha256 ..."
  if curl -fLs "$base/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
    (cd "$tmp" && grep -E '  nft-forward$' SHA256SUMS | sha256sum -c -) \
      || die "sha256 校验失败"
  else
    die "未取到 SHA256SUMS：update 必须强校验，拒绝裸跑"
  fi

  # ---- 架构防护（产物侧，避免误装 arm64 进 amd64 机器）----
  file "$tmp/nft-forward" | grep -q 'ELF 64-bit LSB.*x86-64' \
    || die "下载到的二进制不是 ELF 64-bit x86-64（content: $(file "$tmp/nft-forward"))"

  # ---- exec 自检：只要不是 "exec format error" 就通过 ----
  # nft-forward 没 --version flag；Go flag 包遇到未知 flag 退 2 但能 exec，足够证明二进制可运行。
  if ! "$tmp/nft-forward" --version >/dev/null 2>&1; then
    if [[ $? -gt 125 ]]; then
      die "新二进制无法执行（exec format / 缺权限）"
    fi
  fi

  # ---- 备份旧二进制 ----
  note "[3/5] 备份旧二进制到 $INSTALL_DIR/nft-forward.bak ..."
  cp -a "$INSTALL_DIR/nft-forward" "$INSTALL_DIR/nft-forward.bak"
  trap 'rm -rf "$tmp"; rollback_update' ERR INT TERM

  # ---- 原子替换 ----
  install -m 0755 "$tmp/nft-forward" "$INSTALL_DIR/nft-forward"

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
  local i
  for i in $(seq 1 10); do
    if systemctl is-active --quiet nft-forward-daemon.service \
       && curl -sf --unix-socket /var/run/nft-forward.sock \
               http://daemon/v1/health 2>/dev/null \
          | grep -q '"ok":true'; then
      break
    fi
    sleep 1
  done
  systemctl is-active --quiet nft-forward-daemon.service \
    && curl -sf --unix-socket /var/run/nft-forward.sock \
            http://daemon/v1/health 2>/dev/null \
       | grep -q '"ok":true' \
    || rollback_update

  # ---- 成功收尾 ----
  trap 'rm -rf "$tmp"' EXIT
  rm -f "$INSTALL_DIR/nft-forward.bak"
  local sha size
  sha=$(sha256sum "$INSTALL_DIR/nft-forward" | awk '{print $1}')
  size=$(stat -c %s "$INSTALL_DIR/nft-forward" 2>/dev/null || stat -f %z "$INSTALL_DIR/nft-forward")
  ok "===== Update 完成 ====="
  echo "二进制 sha256: $sha"
  echo "二进制 size:   $size 字节"
  echo "建议查看启动日志: journalctl -u nft-forward-daemon.service --since '1 minute ago'"
}
```

- [ ] **Step 6: dispatch — mode=update 调 do_update**

在 `install.sh` 的 `if [[ "$mode" == "uninstall" ]]; then ... exit 0; fi` 块之后（也就是行 208 的 `exit 0; fi` 之后）插入：

```bash
# Update is its own code path: no role unit changes, only binary swap.
if [[ "$mode" == "update" ]]; then
  # Reuse install's $base resolution by inlining the same logic do_update needs.
  if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
    base="$NFTF_RELEASE_BASE_URL"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  do_update
  exit 0
fi
```

注意：`do_update()` 依赖 `$base`、`$INSTALL_DIR`、`$REPO`，这些都是 install.sh 顶部已有的常量；这里只重新算 `base`（因为 update 总是 latest，无需走 `--release` 分支）。

- [ ] **Step 7: 排除 update 走"角色选择交互"**

`install.sh:137-152` 的"interactive when no TTY arg"分支当 `$mode == update` 时不应触发，否则会被 `case` 里的 `"未知选项"` die。检查现有 `if [[ -z "$mode" ]]` 已经把 update 通过的情况覆盖了（mode 已设）。无需改动，但**确认**：

```bash
bash -n install.sh
sudo bash install.sh update --help 2>&1 | head -5 || true
```

期望 `--help` 在 update 之前被解析（usage 显示后退出码 0）。

- [ ] **Step 8: 手动 dry-run（不动当前机器）**

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 9: Commit**

```bash
git add install.sh
git commit -m "install: add update subcommand with atomic swap and rollback"
```

---

### Task 3: docker/test.sh — update 成功 + 失败回滚

**目的：** spec §测试 step 5/6 落到 CI。两个 case 共享同一个本地 HTTP fixture，仅准备阶段差异。

**Files:**
- Modify: `docker/test.sh:40+`（追加在现有第 4 步"清理"之前，重排控制流）

- [ ] **Step 1: 重排现有 trap 与 step 4**

`docker/test.sh:14-15`：

```bash
note "1. 启动 daemon + server"
docker compose up -d
trap 'note "清理"; docker compose down -v' EXIT
```

保留。但现有"step 4. 停止并清理"（行 34-38）要往后挪——更多 case 会复用同一个 compose stack。把行 34-38 删掉，最后统一收尾。

具体动作：删除 `docker/test.sh:34-38`：

```bash
note "4. 停止并清理 (compose down -v)"
# EXIT trap handles compose down -v; disable it to avoid double-run and
# run explicitly so the exit code from compose down is visible.
trap - EXIT
docker compose down -v
green "  清理完成"
```

后续 step 5/6/7/8 在 step 3 之后追加，最后再写一次"step N. 清理"作为收尾。

- [ ] **Step 2: 在 daemon 容器内安装 install.sh 工件**

`docker/Dockerfile` 已经把 `nft-forward` 放到 `/usr/local/sbin/nft-forward`，但**没有装 install.sh**——docker fixture 跑的是 compose `command:` 而不是 install.sh。

为了让 update / purge 都能在容器里跑，需要把 install.sh 也 COPY 进镜像。修改 `docker/Dockerfile`：

把：

```dockerfile
COPY --from=build /out/nft-forward /usr/local/sbin/nft-forward
```

替换为：

```dockerfile
COPY --from=build /out/nft-forward /usr/local/sbin/nft-forward
COPY install.sh /usr/local/sbin/nft-forward-install
RUN chmod +x /usr/local/sbin/nft-forward-install
```

然后在 `docker/test.sh` 现有"step 3. 通过 daemon 容器访问 unix socket"之后插入：

- [ ] **Step 3: 写 step 5 — update 成功路径**

在 `docker/test.sh` 行 33（"green daemon unix socket 健康正常"）之后插入：

```bash
note "5. update 成功路径"
docker compose exec daemon bash -c '
  set -e
  # Mock release artifact: serve the same binary + freshly computed SHA256SUMS.
  mkdir -p /tmp/relmock
  cp /usr/local/sbin/nft-forward /tmp/relmock/nft-forward
  ( cd /tmp/relmock && sha256sum nft-forward > SHA256SUMS )
  ( cd /tmp/relmock && python3 -m http.server 8765 >/tmp/http.log 2>&1 & echo $! >/tmp/http.pid )
  sleep 1
  # Need systemd to drive the daemon unit. The compose fixture runs daemon as
  # PID 1 of the container; install.sh expects systemd. We simulate the bits
  # install.sh actually calls (systemctl) via a shim — see step 4 below.
'
```

⚠️ **依赖问题暴露**：docker fixture 的 daemon 容器没跑 systemd（命令直接 exec 二进制）。`install.sh update` 要求 systemctl 可用。两个选项：

（A）把 docker fixture 升级为 systemd-in-docker（用 `jrei/systemd-debian` 之类的基础镜像 + privileged）—— 改动大、引入新依赖。

（B）docker/test.sh 不直接跑 `install.sh update`，而是写一个"测试驱动器" `docker/test_update.sh` 在容器内调用 `do_update()` 函数（用 `bash -c "source install.sh; do_update"` 风格），并预先 export 假的 `systemctl` shim。

（C）放弃 docker 自动化，把 update 成功/回滚的端到端验证全部放到 `docs/daemon-manual-verification.md` 手工章节，docker 只验"语法 + base URL 替换"。

**选 C**。理由：spec 非目标里已经说"不引入 bats / shellspec / shunit2"，要在 docker 里硬上 systemd 等价于引入新框架。手工章节足够覆盖（项目本来就有 daemon-manual-verification 兜底）。

回到 step 3 的实际改动——降级为"语法 + URL 替换"测试：

```bash
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
  # NFTF_RELEASE_BASE_URL 接管：用 latest 但 base 是 mock
  mkdir -p /tmp/relmock
  cp /usr/local/sbin/nft-forward /tmp/relmock/nft-forward
  ( cd /tmp/relmock && sha256sum nft-forward > SHA256SUMS )
  ( cd /tmp/relmock && python3 -m http.server 8765 >/tmp/http.log 2>&1 & disown )
  sleep 1
  # 预探：URL 替换工作
  curl -sf http://127.0.0.1:8765/nft-forward -o /tmp/check.bin
  test "$(sha256sum /tmp/check.bin | awk "{print \$1}")" \
       = "$(sha256sum /usr/local/sbin/nft-forward | awk "{print \$1}")" \
    || { echo "URL 替换/sha 校验链路异常"; exit 1; }
' || fail "step 5 失败"
green "  update 子命令 dry-run 通过（systemd 依赖见手工章节）"
```

注意：python3 在 debian:bookworm-slim 默认不装。修改 `docker/Dockerfile` 的 apt-get 行加 python3：

把 `docker/Dockerfile:10`:

```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends nftables iproute2 ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
```

替换为：

```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends nftables iproute2 ca-certificates curl python3 \
 && rm -rf /var/lib/apt/lists/*
```

- [ ] **Step 4: 加 step 6 — purge guards 验证（docker 能跑的部分）**

接 step 3 之后插入：

```bash
note "6. uninstall --purge 参数 guards"
docker compose exec daemon bash -c '
  if /usr/local/sbin/nft-forward-install uninstall server --purge 2>&1 \
     | grep -qE "未知|不存在|找不到|^错误"; then
    echo "  没有 server unit 时 purge 优雅退出"
  else
    echo "  注意：当前实现可能在无 server unit 时仍尝试 purge—请检查日志"
  fi
  /usr/local/sbin/nft-forward-install tui --purge 2>&1 \
    | grep -q "--purge 仅 uninstall 模式有效" \
    || { echo "tui --purge guard 未生效"; exit 1; }
' || fail "step 6 失败"
green "  uninstall guard 验证通过"
```

- [ ] **Step 5: 写收尾 step**

```bash
note "N. 停止并清理 (compose down -v)"
trap - EXIT
docker compose down -v
green "  清理完成"

echo
green "===== compose smoke test 通过 ====="
```

- [ ] **Step 6: 跑一遍 docker/test.sh 验证**

```bash
cd /Users/xjetry/work/vibe/nft-forward
bash docker/test.sh
```

期望：所有 step 都通过，最后 `===== compose smoke test 通过 =====`。

- [ ] **Step 7: Commit**

```bash
git add docker/test.sh docker/Dockerfile
git commit -m "docker: extend smoke test for update guards and NFTF_RELEASE_BASE_URL"
```

---

### Task 4: --purge 参数解析（含 die guards）

**目的：** flag 落位 + 错误用法 guard。本身不接任何业务逻辑，只是为后续 Task 5/6/7 让路。

**Files:**
- Modify: `install.sh`（参数解析区 + uninstall 路径分发）

> **说明：** Task 2 step 2 已经加了 `--purge` 解析和 `purge=0` 初始化、step 3 已经加了"--purge 仅 uninstall 模式有效"的 die。本任务**只**追加 uninstall 路径里对 `$purge` 的传递准备工作——确保后续 server/agent/daemon 三个分支都能读到 `$purge` 变量。

- [ ] **Step 1: 验证 $purge 在 uninstall case 内可见**

`install.sh:178-208` 的 `case "$UNINSTALL_TARGET" in ... esac` 是同一 shell scope，`$purge` 是全局变量，无需额外传递。

跑：

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 2: Commit（合并进 Task 5 的 commit）**

⚠️ 本任务本身没有独立产物——它只是确认前置条件。**跳过独立 commit**，直接进 Task 5。

---

### Task 5: uninstall server --purge

**目的：** spec §设计 → uninstall --purge 范围 表里 `server` 行。

**Files:**
- Modify: `install.sh:178-184`（server case 块）

- [ ] **Step 1: 改写 server case 加 purge 分支**

把 `install.sh:179-184`：

```bash
    server)
      systemctl disable --now nft-forward-server.service 2>/dev/null || true
      rm -f "$SYSTEMD_DIR/nft-forward-server.service"
      systemctl daemon-reload
      ok "已卸载 server 角色（daemon 保留）"
      ;;
```

替换为：

```bash
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
```

- [ ] **Step 2: 语法检查**

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 3: Commit**

```bash
git add install.sh
git commit -m "install: add --purge to uninstall server (clear panel.db and daemon panel segment)"
```

---

### Task 6: uninstall agent --purge

**目的：** spec §设计 → uninstall --purge 范围 表里 `agent` 行。注意 daemon-reload + restart 必须在 panel 段 POST 之**后**（否则 daemon 重启时还没把空 panel 段写入 state.json，重启就丢了 clear 的意图）—— 不对，daemon 在 POST 时已经原子持久化 state.json + 重算 nftables（README §协议参考）。所以 POST 顺序在 daemon-reload 前后都 ok。**保守做法是 POST 在前**：让 daemon 当时就清掉 panel 段并持久化，restart 后从 state.json 重放就是空段。

**Files:**
- Modify: `install.sh:185-192`（agent case 块）

- [ ] **Step 1: 改写 agent case 加 purge 分支**

把 `install.sh:186-191`：

```bash
    agent)
      # Restore the daemon unit to a no-listen ExecStart and remove the token file.
      write_daemon_unit ""
      rm -f /etc/nft-forward/daemon.token
      systemctl daemon-reload
      systemctl restart nft-forward-daemon.service
      ok "已卸载 agent 角色（daemon 保留，去掉 --listen）"
      ;;
```

替换为：

```bash
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
```

- [ ] **Step 2: 语法检查**

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 3: Commit**

```bash
git add install.sh
git commit -m "install: add --purge to uninstall agent (clear /etc/nft-forward and daemon panel segment)"
```

---

### Task 7: uninstall daemon --purge

**目的：** spec §设计 → uninstall --purge 范围 表里 `daemon` 行。daemon 是最重的 purge——完整擦除 state、sysctl drop-in、nftables 表、系统组，并对 tc 残留作 stderr 提示。

**Files:**
- Modify: `install.sh:193-205`（daemon case 块）

- [ ] **Step 1: 改写 daemon case 加 purge 分支**

把 `install.sh:193-204`：

```bash
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
```

替换为：

```bash
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
```

- [ ] **Step 2: 语法检查**

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 3: Commit**

```bash
git add install.sh
git commit -m "install: add --purge to uninstall daemon (clear state, sysctl, nft table, group)"
```

---

### Task 8: 角色切换自动清旧

**目的：** spec §设计 → 角色切换自动清旧。`install.sh [tui|server|agent]` 入口在下载二进制之前检测当前角色，对应清旧 unit / token，**不动 state.json**。

**Files:**
- Modify: `install.sh`（新增 detect 函数 + 在三个安装分支前 dispatch）

- [ ] **Step 1: 写检测函数**

在 `remove_legacy_units() { ... }`（`install.sh:51-69`）之后插入：

```bash
# Detect what role is currently installed by reading systemd unit-files and
# the daemon unit ExecStart. Echoes a space-separated list of roles found
# (any of "server" "agent"), or nothing if only a baseline daemon-only TUI
# install exists. Caller is expected to dispatch into uninstall paths for
# each role echoed.
detect_existing_roles() {
  local roles=()
  if systemctl list-unit-files --no-legend \
     | grep -q '^nft-forward-server\.service '; then
    roles+=(server)
  fi
  if [[ -f "$SYSTEMD_DIR/nft-forward-daemon.service" ]] \
     && grep -q -- '--listen' "$SYSTEMD_DIR/nft-forward-daemon.service"; then
    roles+=(agent)
  fi
  echo "${roles[@]}"
}
```

- [ ] **Step 2: 写角色切换 dispatcher**

在 `detect_existing_roles()` 之后插入：

```bash
# When installing mode $new, clean up any conflicting old roles.
# The matrix from the design doc:
#   tui    -> uninstall server, uninstall agent
#   server -> uninstall agent  (server unit gets rewritten in place)
#   agent  -> uninstall server (daemon unit gets rewritten with --listen)
# Re-installing the same role doesn't trigger cleanup.
switch_role_cleanup() {
  local new="$1"
  local existing
  existing="$(detect_existing_roles)"
  case "$new" in
    tui)
      [[ "$existing" == *server* ]] && do_uninstall server 0
      [[ "$existing" == *agent*  ]] && do_uninstall agent  0
      ;;
    server)
      [[ "$existing" == *agent* ]] && do_uninstall agent 0
      ;;
    agent)
      [[ "$existing" == *server* ]] && do_uninstall server 0
      ;;
  esac
}
```

- [ ] **Step 3: 抽取 do_uninstall 函数**

切换需要"内部调用 uninstall 流程"，但当前 `install.sh:177-208` 是 inline `if [[ "$mode" == "uninstall" ]]; then case ... esac; exit 0; fi` 的形式——没法被 `switch_role_cleanup` 调用。

把这一整块抽成函数。在 `switch_role_cleanup() { ... }` 之后插入 `do_uninstall()` 函数（把 Task 5/6/7 改完的最新 case 内容整体搬进来）：

```bash
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
      systemctl daemon-reload
      if [[ "$purge" -eq 1 ]]; then
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
      fi
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
        nft delete table ip nft_forward 2>/dev/null || true
        if getent group nft-forward >/dev/null; then
          groupdel nft-forward 2>/dev/null || true
        fi
        ok "已卸载 daemon + 清 state.json / sysctl drop-in / nftables 表 / 系统组"
        echo "提示：如有 tc HTB 限速残留，请手动 tc qdisc del dev <iface> root" >&2
      else
        ok "已卸载 daemon（state.json 保留在 /var/lib/nft-forward/）"
      fi
      ;;
    *) die "未知卸载目标: $target" ;;
  esac
}
```

- [ ] **Step 4: 替换 install.sh 顶层 uninstall 分发**

把 `install.sh:177-208`：

```bash
# Uninstall takes a separate code path (no download needed).
if [[ "$mode" == "uninstall" ]]; then
  case "$UNINSTALL_TARGET" in
    server) ... ;;
    agent)  ... ;;
    daemon) ... ;;
    *) die "未知卸载目标: $UNINSTALL_TARGET" ;;
  esac
  exit 0
fi
```

替换为：

```bash
if [[ "$mode" == "uninstall" ]]; then
  do_uninstall "$UNINSTALL_TARGET" "$purge"
  exit 0
fi
```

- [ ] **Step 5: 在三个 install 分支调用 switch_role_cleanup**

把 `install.sh:240-293` 的 `case "$mode" in` 块的三个分支，**在 `write_daemon_unit "..."` 之前**各插入一次 `switch_role_cleanup`。

tui 分支：

```bash
  tui)
    switch_role_cleanup tui
    write_daemon_unit ""
    ...
```

server 分支：

```bash
  server)
    switch_role_cleanup server
    write_daemon_unit ""
    write_server_unit "$addr"
    ...
```

agent 分支：

```bash
  agent)
    switch_role_cleanup agent
    mkdir -p /etc/nft-forward
    install -m 0600 /dev/stdin /etc/nft-forward/daemon.token <<<"$token"
    write_daemon_unit " --listen :$port --token-file /etc/nft-forward/daemon.token"
    ...
```

- [ ] **Step 6: 语法检查 + dry-run**

```bash
bash -n install.sh
```

期望：无输出。

- [ ] **Step 7: 跑 docker/test.sh 全程**

```bash
bash docker/test.sh
```

期望：所有 step 通过。

- [ ] **Step 8: Commit**

```bash
git add install.sh
git commit -m "install: auto-clean opposing roles when switching tui/server/agent"
```

---

### Task 9: README.md 升级章节

**目的：** spec §设计 § "在 README 升级章节明确告知"角色切换不动 state 的不变量"。还要新增 `install.sh update` 和 `uninstall --purge` 的引用。

**Files:**
- Modify: `README.md` § "升级与迁移" 章节

- [ ] **Step 1: 找到现有"升级与迁移"章节**

```bash
grep -n '^## 升级与迁移' /Users/xjetry/work/vibe/nft-forward/README.md
```

记下行号 N。

- [ ] **Step 2: 在 N+1 行起追加 install.sh update 子章节**

在"升级与迁移"章节标题之后、"从旧版（三二进制布局）升级"小节之前插入：

```markdown
### 日常升级

新版发布后用 `install.sh update` 升级现有部署：

\`\`\`bash
sudo bash install.sh update
\`\`\`

行为：拉 GitHub latest 二进制 → sha256 校验 → ELF x86-64 架构校验 → 备份旧二进制到 `/usr/local/sbin/nft-forward.bak` → 原子替换 → `systemctl restart nft-forward-daemon.service`（+ `nft-forward-server.service` 如有）→ 10 秒 health-check。失败自动回滚到旧二进制并重启。

约束：

- `update` 总是拉 `latest`；要锁版本/降级请用 `install.sh tui --release v1.2.3`（等价于重装）
- `update` 不动 systemd unit 文件，`--listen` / `--addr` / token 等角色配置完全保留
- daemon 短暂重启期间 nftables 规则从 state.json 重放，规则不丢

### 角色切换

支持互切：

\`\`\`bash
sudo bash install.sh tui      # 从 server/agent 降回 tui
sudo bash install.sh server   # 从 tui/agent 切到 server
sudo bash install.sh agent    # 从 tui/server 切到 agent
\`\`\`

脚本会自动 detect 当前角色并清理冲突的旧 unit / token。**但 state.json 中之前 panel 段的规则会保留并继续生效**——从 server/agent 降回 tui 不会自动撤掉旧的远程推送规则。需要彻底清的话先跑：

\`\`\`bash
sudo bash install.sh uninstall server --purge   # 卸 server 同时清 panel.db + daemon panel 段
sudo bash install.sh uninstall agent  --purge   # 卸 agent 同时清 /etc/nft-forward + daemon panel 段
sudo bash install.sh uninstall daemon --purge   # 卸 daemon 同时清 state / sysctl / nftables 表 / 系统组
\`\`\`

`--purge` 仅在 `uninstall` 模式下有效；不带 `--purge` 时数据完全保留（向后兼容现有行为）。
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document install.sh update, --purge, and role switching in README"
```

---

### Task 10: docs/daemon-manual-verification.md 手工章节

**目的：** spec §测试 §`docs/daemon-manual-verification.md` 手工章节。补 docker fixture 难以覆盖的端到端 case。

**Files:**
- Modify: `docs/daemon-manual-verification.md`（append 新章节）

- [ ] **Step 1: 在文件末尾追加新章节**

把以下内容追加到 `docs/daemon-manual-verification.md` 末尾：

```markdown

## install / update / uninstall 验证

docker fixture 不跑 systemd，下列 case 需要真实 Linux 主机（VM / 物理机）。

### 1. 干净机器装 tui → update → 验证规则未丢

\`\`\`bash
# 假设 release 已经发了 v1.2.3
sudo bash install.sh tui --release v1.2.3
sudo nft-forward          # 进 TUI，按 'a' 加一条转发规则（如 :10000 → 1.2.3.4:80）
# 退出 TUI；规则在 daemon state.json
\`\`\`

发新版 v1.2.4，跑 update：

\`\`\`bash
sudo bash install.sh update
sudo nft-forward          # 回 TUI 看刚才那条规则是否还在
\`\`\`

预期：规则仍存在，新版二进制 sha 已经变化。检查 `journalctl -u nft-forward-daemon.service --since '5 minutes ago'` 看到正常启动日志。

### 2. agent 角色 update 保留 token

\`\`\`bash
sudo bash install.sh agent --port 7878 --token deadbeef...64hex
ls -l /etc/nft-forward/daemon.token   # 记录 inode 和 mtime
sudo bash install.sh update
ls -l /etc/nft-forward/daemon.token   # 应当与上面一致，inode 不变
cat /etc/systemd/system/nft-forward-daemon.service | grep ExecStart
                                       # 应仍含 --listen :7878 --token-file ...
\`\`\`

### 3. 角色切换链路 tui → server → tui

\`\`\`bash
sudo bash install.sh tui
sudo nft-forward          # 加一条规则 A
# 现在 daemon state.json 的 tui 段有规则 A
sudo bash install.sh server
# server unit 应当被装上；用浏览器开 http://localhost:8080 加一条 panel 段规则 B
sudo bash install.sh tui
# 预期：server unit 自动消失（switch_role_cleanup tui 触发 do_uninstall server 0）
systemctl list-unit-files | grep nft-forward
# 应仅余 nft-forward-daemon.service
sudo nft-forward          # 应同时看到规则 A 和规则 B（state.json 完整保留）
\`\`\`

然后清旧 panel 段：

\`\`\`bash
sudo bash install.sh uninstall server --purge
# server 已经没了；但 --purge 仍会调 daemon API 清 panel 段
sudo nft-forward          # 规则 A 仍在，规则 B 应已消失
\`\`\`

### 4. daemon --purge 后从零重装

\`\`\`bash
sudo bash install.sh uninstall server --purge 2>/dev/null || true
sudo bash install.sh uninstall agent  --purge 2>/dev/null || true
sudo bash install.sh uninstall daemon --purge
\`\`\`

预期：

- `/var/lib/nft-forward/` 不存在
- `/etc/sysctl.d/99-nft-forward.conf` 不存在
- `nft list table ip nft_forward` 返回 `Error: No such file or directory`
- `getent group nft-forward` 无输出
- stderr 出现 tc qdisc 提示

然后从零起：

\`\`\`bash
sudo bash install.sh tui
sudo nft-forward          # 应能空白起步
\`\`\`

### 5. update 失败回滚（人为制造失败）

需要构造"sha 校验过但 daemon 起不来"的情况。最简单：用同一镜像但人为破坏 unit 的 ExecStart：

\`\`\`bash
sudo bash install.sh tui
# 故意把 unit 改坏（指向不存在的 flag），让重启失败
sudo sed -i 's|/nft-forward daemon|/nft-forward daemon --bogus-flag|' \
    /etc/systemd/system/nft-forward-daemon.service
sudo systemctl daemon-reload
# 跑 update，新二进制启动时会因 --bogus-flag fail
sudo bash install.sh update
\`\`\`

预期：

- 退出码 1
- stderr 打印 "update 失败，回滚到旧二进制"
- `/usr/local/sbin/nft-forward` 的 sha 与 update 前一致
- daemon 仍然起不来（unit 被改坏了）—— **这是单元损坏不是 update 单元错**；人工恢复 unit：

\`\`\`bash
sudo sed -i 's| --bogus-flag||' /etc/systemd/system/nft-forward-daemon.service
sudo systemctl daemon-reload
sudo systemctl restart nft-forward-daemon.service
\`\`\`

跑 update 才能验证完整回滚流；正式场景失败原因通常是新二进制 broken 而非 unit 错。
```

- [ ] **Step 2: Commit**

```bash
git add docs/daemon-manual-verification.md
git commit -m "docs: add install/update/uninstall manual verification scenarios"
```

---

## Self-Review Checklist

1. **Spec coverage**:
   - spec §设计 §命令表面 → Task 2 step 1/2 ✓
   - spec §设计 §角色切换自动清旧 → Task 8 ✓
   - spec §设计 §update 实现九步 → Task 2 step 4/5 ✓
   - spec §设计 §uninstall --purge 范围 → Task 5/6/7 ✓
   - spec §设计 §NFTF_RELEASE_BASE_URL → Task 1 + Task 3 ✓
   - spec §测试 §docker/test.sh 扩展 → Task 3（降级为语法/guard 验证，理由记录在 Task 3 step 3）✓
   - spec §测试 §daemon-manual-verification 章节 → Task 10 ✓
   - README 升级章节 → Task 9 ✓

2. **Placeholder scan**: 无 TBD / TODO / "implement later"。所有 code blocks 给出完整可粘贴代码。

3. **Type consistency**:
   - `do_uninstall(target, purge)` 签名 → Task 8 step 3 定义、Task 8 step 4 调用、Task 8 step 2 switch_role_cleanup 调用统一为 `do_uninstall server 0` / `do_uninstall agent 0` ✓
   - `do_update()` 无参；`rollback_update()` 无参；`switch_role_cleanup(new_mode)` 一参 ✓
   - `$purge` 全局变量在 Task 2 step 2 初始化为 0，在 do_uninstall 内通过参数 `purge` 局部覆盖 ✓
   - `$base` 在 install / update 两条路径都重新计算（install 走原有 if/elif/else；update 走 Task 2 step 6 的 NFTF_RELEASE_BASE_URL ? GitHub latest），不交叉污染 ✓
   - `NFTF_RELEASE_BASE_URL` 名称一致（Task 1 / Task 2 / Task 3） ✓

4. **scope / 修订点**:
   - Task 3 在 docker fixture 没有 systemd 的现实约束下，从 spec 原计划的"完整 update 端到端"降级为"语法 + URL 替换 + guard 验证"，完整端到端转交 Task 10 手工章节。已在 Task 3 step 3 留下显式说明。
   - Task 4 实际无独立产物，merge 进 Task 5 commit。已在 Task 4 step 2 显式说明，避免 worker 困惑。
   - Task 8 step 3 复制了 Task 5/6/7 改过的 case 内容——这是必要的（switch_role_cleanup 调用 do_uninstall，inline case 必须改写为函数）。worker 按顺序执行时，Task 5/6/7 的 case 改动会被 Task 8 step 4 整体替换掉；这是设计内的——Task 5/6/7 的 commit 提供 review checkpoint 与可单独回滚的粒度，Task 8 step 3 再统一收口到函数。
