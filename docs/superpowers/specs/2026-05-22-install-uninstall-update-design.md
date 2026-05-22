# install / uninstall / update 完整化

## 背景

`install.sh` 当前已经覆盖：

- `install.sh [tui|server|agent]` —— 下载 amd64 二进制、写 systemd unit、`enable --now`
- `install.sh uninstall <server|agent|daemon>` —— 按角色拆解卸载，daemon 必须最后卸

存在三个 gap：

1. **没有 update 路径**。重跑 `install.sh tui` 实际上会覆盖 `/usr/local/sbin/nft-forward`（`install -m 0755`），但这是侧门——没有架构防护、没有备份、新二进制起不来时没有回滚。
2. **角色切换不干净**。从 `tui` 升 `server`/`agent` 时一切顺畅；反向降回（`server`/`agent` → `tui`）以及 `server` ↔ `agent` 互切时，旧角色的 unit 文件 / token 文件不会被清理，导致两个 unit 共存或孤儿 token。
3. **uninstall 没有强清开关**。state.json / panel.db / nftables 表 / 系统组等运行时残留不会随 `uninstall` 删除，用户需要手动 `rm`。

## 目标

- 新增 `install.sh update`：拉 latest 二进制原子替换、自动重启相关 unit、health-check 失败自动回滚、架构防护。
- 新增 `install.sh uninstall <角色> --purge`：按角色 scope 一键清残留数据。
- 修补角色切换：`install.sh [tui|server|agent]` 入口处自动检测并清理旧角色 unit / token（不动 state.json）。
- 全部在 `install.sh` 内完成，**不动 Go 代码**。

## 非目标

- **不**把这些能力迁移到 `nft-forward` 二进制子命令。bootstrap 永远需要外部脚本，让 Go 重写一遍 systemd unit 模板/角色识别/包管理代码是把代码量翻倍但能力不增加。
- **update 不支持 `--release <tag>`**。锁版本/降级场景请用 `install.sh <角色> --release <tag>`（等价于重装）。
- **update 不做 sha skip**。下载后即使新旧二进制 sha 一致也照常 `systemctl restart`；保持代码路径单一。
- **update 不做 self-update of install.sh 自身**。脚本自身更新交给 README 引导用户重新 `curl` 最新版。
- **purge 不主动关 `net.ipv4.ip_forward`**。只删 `/etc/sysctl.d/99-nft-forward.conf`（reboot 后回到内核默认），当前运行时的 ip_forward 保留给系统管理员决定，避免误伤其它依赖转发的服务。
- **purge 不主动清 tc HTB 残留**。data-plane iface 名由 daemon 自动探测，install.sh 不掌握；硬猜 iface 容易误删用户其它 HTB 规则。改用提示而非动作。
- **不引入 bats / shellspec 等 shell 测试框架**。沿用 `docker/test.sh` + `docs/daemon-manual-verification.md` 的既有格局。

## 设计

### 命令表面

```text
nft-forward 一键安装/卸载/升级脚本

用法:
  install.sh [tui|server|agent|update|uninstall] [选项]

模式:
  tui                单机 TUI（已有；入口处自动清旧角色）
  server             控制面板（已有；入口处自动清旧角色）
  agent              受控节点（已有；入口处自动清旧角色）
  update             【新增】拉 latest 二进制原子替换 + restart + 失败回滚
  uninstall <角色>   按角色卸载（已有）；可带 --purge

选项:
  --addr ADDR        server 监听地址（已有）
  --port PORT        agent 监听端口（已有）
  --token TOKEN      agent bearer token（已有）
  --release VER      install 模式可锁版本（已有）；update 模式传入会 die
  --purge            【新增】仅 uninstall 模式有效；按角色 scope 清残留
  -h, --help

示例:
  sudo install.sh update                       # 升级到 latest
  sudo install.sh uninstall server --purge     # 卸 server + 删 panel.db
  sudo install.sh uninstall daemon --purge     # 卸 daemon + 删 state + nftables 表
```

兼容性：现有 `install.sh tui|server|agent|uninstall` 的语义和参数完全不变，新增能力都通过新子命令或新 flag 暴露。

### 角色切换自动清旧

`install.sh [tui|server|agent]` 在下载二进制之前先 detect 当前角色状态，调用对应的内部 `uninstall` 路径清掉旧角色 unit / token。**不带 `--purge`**——state.json / panel.db 不动，用户的规则数据保留。

**检测信号**：

| 信号 | 来源 |
|---|---|
| server unit 是否存在 | `systemctl list-unit-files --no-legend \| grep -q '^nft-forward-server\.service '` |
| daemon 是否带 `--listen`（即 agent 角色） | `grep -q -- '--listen' "$SYSTEMD_DIR/nft-forward-daemon.service"`（仅在 unit 文件存在时执行） |

**切换矩阵**：

| 新装 | 检测到 | 自动动作（内部调用 uninstall，不带 purge） |
|---|---|---|
| `tui` | server unit 存在 | uninstall server |
| `tui` | daemon unit 含 `--listen` | uninstall agent |
| `server` | daemon unit 含 `--listen` | uninstall agent |
| `agent` | server unit 存在 | uninstall server |

**重装同角色**（tui→tui、server→server、agent→agent）不触发清理。

**重要不变量**：自动清旧只动 unit/token，不动 `/var/lib/nft-forward/state.json`。这意味着从 server/agent 降回 tui 时，旧的 `panel` 段规则保留在 daemon 内继续生效——用户若想清这些规则，必须显式跑 `install.sh uninstall <旧角色> --purge` 后再 install。spec 在 README 升级章节明确告知这一点。

### update 实现

执行流程，任何一步失败立即触发回滚：

**1. 前置探测**
- `/usr/local/sbin/nft-forward` 不存在 → die（"未安装"）
- `nft-forward-daemon.service` 不在 unit-files 列表 → die（同因）
- 用户传了 `--release` → die（"update 只拉 latest，要锁版本请用 install"）

**2. 架构防护**
- `uname -m` 非 `x86_64`/`amd64` → die
- 下载完成后跑 `file "$tmp/nft-forward"`，输出含 `ELF 64-bit LSB executable, x86-64` 才继续；不匹配 → die，不进入替换

**3. 下载 + 校验**
- 沿用 `install.sh` 现有的 `tmp="$(mktemp -d)"` + `curl -fL --progress-bar` + `sha256sum -c` 逻辑（行 219-232）
- `base="https://github.com/$REPO/releases/latest/download"`（`RELEASE` 强制为 `latest`）
- SHA256SUMS 校验失败 → die
- 通过校验后跑 `"$tmp/nft-forward" --help`（main.go 没有 `--help` flag，Go 的 flag 包默认行为是打印 usage 退 2，足够证明二进制能被执行——非 0 退出码不是失败信号，**只要 exec 不报 "exec format error" 就算通过**）

**4. 备份旧二进制**
- `cp -a /usr/local/sbin/nft-forward /usr/local/sbin/nft-forward.bak`
- 设 trap：`trap 'rollback' ERR INT TERM`，`rollback()` 函数定义见步骤 8

**5. 原子替换**
- `install -m 0755 "$tmp/nft-forward" /usr/local/sbin/nft-forward`
- `install` 命令本身原子（`open(O_CREAT) + write + rename`），运行中的 daemon 进程内存映像不受影响

**6. 重启 unit**
- `systemctl daemon-reload`（unit 文件没改，开销低，无害）
- `systemctl restart nft-forward-daemon.service`
- 如果 `nft-forward-server.service` 在 unit-files 列表里 → `systemctl restart nft-forward-server.service`
- 不显式 restart 配 `--listen` 的 daemon（agent 角色）——它就是 daemon unit 本体

**7. health-check（10 秒预算，每秒轮询）**

```bash
ok_count=0
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
[[ "$ok_count" -eq 1 ]] || rollback
```

**8. 回滚（rollback 函数）**

```bash
rollback() {
  echo "update 失败，回滚到旧二进制" >&2
  mv -f /usr/local/sbin/nft-forward.bak /usr/local/sbin/nft-forward
  systemctl restart nft-forward-daemon.service
  if systemctl list-unit-files --no-legend \
     | grep -q '^nft-forward-server\.service '; then
    systemctl restart nft-forward-server.service
  fi
  exit 1
}
```

回滚后不再 health-check。旧版本之前就是 working 的；若旧版本起不来，说明更深层环境问题，留 `.bak`-restored 状态给人工介入，stderr 明确打印。

**9. 成功收尾**
- `rm -f /usr/local/sbin/nft-forward.bak`
- `ok` 打印新二进制 sha256、size、提示用户检查 `journalctl -u nft-forward-daemon.service`

**关键不变量**：
- update 不动 unit 文件内容 → 用户的 `--listen`、`--addr` 配置不会丢
- update 不动 `/etc/nft-forward/daemon.token`
- daemon 重启期间 nftables 规则从 state.json 重放（既有的 owner-segmented 恢复路径），转发短暂中断（< 5s 量级），规则不丢

### uninstall `--purge` 范围

`--purge` 仅在 `uninstall` 模式下有效；其它模式传入 → die（"--purge 仅 uninstall 模式有效"）。

每个角色的 `uninstall` **不带 purge** 时行为完全不变（保持向后兼容）。`--purge` 在原动作之上叠加额外清理：

| 角色 | `uninstall` 现有动作 | `--purge` 额外动作 |
|---|---|---|
| `server` | disable+stop `nft-forward-server.service`、rm unit、daemon-reload | (1) **先**清 daemon 的 panel 段：`curl --unix-socket /var/run/nft-forward.sock -X POST -H 'Content-Type: application/json' http://daemon/v1/ruleset/panel -d '{"rules":[]}'`（best-effort，daemon 若不响应记 warning 不阻塞）<br>(2) `rm -f /var/lib/nft-forward/panel.db panel.db-wal panel.db-shm` |
| `agent` | 把 daemon unit ExecStart 改回无 `--listen`、rm `/etc/nft-forward/daemon.token`、daemon-reload + restart daemon | (1) **先**清 daemon 的 panel 段（同上 API，在 daemon-reload + restart 之前发，确保持久化到 state.json）<br>(2) `rm -rf /etc/nft-forward/` |
| `daemon` | 前置 guard：要求先卸 server；disable+stop daemon unit、rm unit、rm 二进制、daemon-reload | (1) `rm -rf /var/lib/nft-forward/`<br>(2) `rm -f /etc/sysctl.d/99-nft-forward.conf`<br>(3) `nft delete table ip nft_forward 2>/dev/null \|\| true`（best-effort 清残留表）<br>(4) `getent group nft-forward >/dev/null && groupdel nft-forward \|\| true`<br>(5) stderr 提示：若有 tc HTB 限速残留请手动 `tc qdisc del dev <iface> root` |

**关键顺序**：
- server `--purge`：先 disable+stop server unit（让 server 闭嘴）→ rm unit + daemon-reload → 此时再 POST panel rules=[]（避免被 server 的最后一次 push 覆盖）→ rm panel.db*
- agent `--purge`：先 POST panel rules=[] → write_daemon_unit "" → rm token → daemon-reload + restart daemon → rm -rf /etc/nft-forward/
- daemon `--purge`：所有动作都在 daemon stop 之后做

**panel 段清空的语义保证**：`POST /v1/ruleset/panel` 由 daemon 原子地更新 state.json + 重算 nftables 表（README §"协议参考"）。daemon 后续重启从 state.json 重放，panel 段也是空——清空效果持久。

### NFTF_RELEASE_BASE_URL（测试 hook）

新增环境变量 `NFTF_RELEASE_BASE_URL`，当且仅当显式设置时替代默认的 GitHub releases URL。install 和 update 两条下载路径都读这个变量。

```bash
if [[ -n "${NFTF_RELEASE_BASE_URL:-}" ]]; then
  base="$NFTF_RELEASE_BASE_URL"
elif [[ "$RELEASE" == "latest" ]]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$RELEASE"
fi
```

用途：`docker/test.sh` 在容器内启 `python3 -m http.server` 模拟 release artifact，避免 CI 跑外网。不在 README 文档化（只作测试 hook）；root only，攻击面有限（攻击者若已能控制 env 跑 root install.sh，攻击面早就大于这个变量）。

## 测试

### `docker/test.sh` 自动测试（在现有四步后追加）

5. **update 成功路径**
   - 容器内启 `python3 -m http.server 8000`，目录里放 `nft-forward`（同一镜像的二进制）+ 现场生成的 `SHA256SUMS`
   - `NFTF_RELEASE_BASE_URL=http://127.0.0.1:8000 install.sh update`
   - 断言：退出码 0；daemon 仍 active；`/v1/health` 仍 ok；`/usr/local/sbin/nft-forward.bak` 不存在

6. **update 失败回滚**
   - http server 这次返回故意坏的二进制（如 `printf 'broken' > nft-forward`，同步重算 SHA256SUMS 让 sha 校验过——制造"sha 过但 exec 不了/health-check 不过"的失败模式）
   - `install.sh update`
   - 断言：退出码 1；当前 `nft-forward` 的 sha256 与 update 之前一致（已回滚）；daemon 仍 active；`.bak` 不存在（rollback 成功后已 mv 回去）

7. **uninstall server --purge**
   - 装 server 角色；触发 server push 让 daemon 里有几条 panel 段规则；记录 `nft list table ip nft_forward` 的输出
   - `install.sh uninstall server --purge`
   - 断言：`panel.db*` 三个文件都不存在；`state.json` 仍存在；daemon unit 仍 active；`/v1/ruleset` 的 `owners.panel` 字段为空数组

8. **uninstall daemon --purge**（必须在 7 之后执行；server 已不存在）
   - `install.sh uninstall daemon --purge`
   - 断言：`/var/lib/nft-forward/`、`/etc/sysctl.d/99-nft-forward.conf` 都不存在；`nft list table ip nft_forward` 返回 not-found；`getent group nft-forward` 返回空

### `docs/daemon-manual-verification.md` 手工章节

新增章节《install / update / uninstall 验证》，覆盖 docker fixture 难以模拟的场景：

- 干净物理机/VM 装 tui → 跑 update → 验证规则未丢
- agent 角色 update（含 `/etc/nft-forward/daemon.token` 文件保留断言）
- tui → server → tui 切换链路：验证 server unit 自动消失、`panel` 段规则保留（直觉验证）；再跑 `uninstall server --purge` 验证 panel 段被清
- daemon `--purge` 后再跑 `install.sh tui` 是否能从零起干净（exercise 旧 state 不存在的分支）

### 回归

- `go test ./...` 与 `go vet ./...` 不受影响（本方案不动 Go 代码）
- CI 矩阵不扩——目前只发布 amd64，无需 arm64 验证

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| update 期间 daemon 短暂停机 → 转发中断 | 既有 daemon 启动从 state.json 重放，中断窗口 < 5s；不引入新行为。文档明示 |
| 回滚后旧二进制也起不来（极端环境劣化） | trap 走完后 stderr 详细打印 + 留 systemd 状态供 `journalctl` 排查；不强行二次回滚或 reset 状态 |
| daemon `--purge` 后 tc HTB 残留 | install.sh 不掌握 data-plane iface 名；只提示用户手动 `tc qdisc del dev <iface> root` |
| 角色切换不带 `--purge` 时 panel 段规则残留并生效 | spec 明示行为；用户如需"切角色+清旧规则"，必须 `uninstall <旧角色> --purge` 后再 `install.sh <新角色>` |
| `NFTF_RELEASE_BASE_URL` 被滥用注入恶意二进制 | 需 root；不文档化；同等条件下攻击者直接替换 `/usr/local/sbin/nft-forward` 更直接，本变量并非新增攻击面 |
| `/v1/ruleset/panel` POST 在 daemon 已停时失败 | best-effort 处理：失败时打印 warning 不阻塞 purge；后续 `daemon --purge` 会清整个 state.json，最终一致 |
