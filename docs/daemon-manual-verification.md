# Daemon 手动验证

直接用 `curl --unix-socket` 打 daemon 的 unix socket API 端到端验证。daemon 当前不接入 TUI / server / agent — 后者仍走旧路径。

## 前置

- Linux + nftables（与现有 nft-forward 一致）
- Go 1.22+
- root 终端（daemon 必须 root 跑）

## Build

```bash
go build -trimpath -ldflags="-s -w" -o ./build/nft-forward ./cmd/nft-forward
```

## 跑 daemon

```bash
sudo ./build/nft-forward daemon \
    --socket=/tmp/nft-forward.sock \
    --state=/tmp/nft-forward-state.json \
    --group=""
```

预期 stdout：

```
nft-forward daemon: listening on /tmp/nft-forward.sock
```

## 在另一个终端验证

```bash
# 1. health
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/health
# → {"ok":true}

# 2. 取空 segmented ruleset
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"owners":{}}

# 3. 提交一条 tui-owned rule
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rT","proto":"tcp","src_port":19090,"dest_ip":"127.0.0.1","dest_port":22}]}' \
     http://unix/v1/ruleset/tui
# → {"count":1}

# 4. 提交一条 panel-owned rule
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rP","proto":"udp","src_port":19091,"dest_ip":"127.0.0.1","dest_port":53}]}' \
     http://unix/v1/ruleset/panel
# → {"count":1}

# 5. 验证内核确实写入了两条（来自不同 owner 的 merge）
sudo nft list table ip nft_forward | grep -E "1909[01]"
# → 应看到两行：dport 19090 → 127.0.0.1:22 和 dport 19091 → 127.0.0.1:53

# 6. 再 GET 一次 — 应该看到两个 owner segment
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"owners":{"panel":[{...}],"tui":[{...}]}}

# 7. 跨 owner 端口冲突 — 让 panel 抢走 tui 已占的 tcp/19090，应被拒
curl -s -o /dev/null -w '%{http_code}\n' --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"steal","proto":"tcp","src_port":19090,"dest_ip":"2.2.2.2","dest_port":22}]}' \
     http://unix/v1/ruleset/panel
# → 409

# 8. 清空 tui segment（POST 空数组）
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[]}' \
     http://unix/v1/ruleset/tui
# → {"count":0}

# 9. 旧的扁平 POST endpoint 已经下线（returns 410 Gone）
curl -s -o /dev/null -w '%{http_code}\n' --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[]}' \
     http://unix/v1/ruleset
# → 410

# 10. 验证 state 文件 schema = v2 + owner-segmented
cat /tmp/nft-forward-state.json
# → {"version":2,"owners":{"panel":[{...}]}}

# 11. Ctrl-C 终止 daemon → /tmp/nft-forward.sock 应被清理
ls /tmp/nft-forward.sock
# → No such file or directory
```

## Restart 恢复验证

1. 重新启动 daemon（同样的 `--state` 路径）
2. 不发任何 POST，立即 `sudo nft list table ip nft_forward` — 应该看到上一次 GET 看到的所有 owner rule（Bootstrap 从 state.json 重放 merged ruleset）

## 旧 state 文件迁移验证

如果你的机器之前跑过 nft-forward TUI / nft-agent / nft-server，对应的 `rules.json` / `agent-state.json` / `embedded-agent-state.json` 会在 daemon **第一次启动**时被导入到对应 owner segment：

- `/etc/nft-forward/rules.json` → `tui` segment
- `/var/lib/nft-forward/agent-state.json` → `panel` segment
- `/var/lib/nft-forward/embedded-agent-state.json` → `panel` segment（与上一条同时存在时此条优先）

每个被处理过的文件会重命名为 `<原路径>.migrated`（不删，留人工备份）。

后续 daemon 重启不会重复迁移：只要 `--state` 指向的文件已存在，迁移就被跳过。

## Server (panel) + daemon

此场景验证 daemon 与 server（panel web UI）一起工作，展示 TUI、panel、daemon 三者如何并存且互相隔离。

1. 启动 daemon：
   ```bash
   sudo nft-forward daemon
   ```
   预期输出：`nft-forward daemon: listening on /var/run/nft-forward.sock`

2. 启动 server（panel）：
   ```bash
   sudo nft-forward server --addr :8080
   ```
   首次启动会生成 admin 密码并打印到 stdout。

3. 打开浏览器访问 `http://host:8080`，使用 admin 密码登录。

4. 在 panel 确认 `localhost` 节点已自动注册，其地址为 `unix:///var/run/nft-forward.sock`。

5. 通过 panel UI 在 localhost 节点上创建一条 forward。

6. 在终端验证 panel 创建的 forward 出现在 daemon 的 `panel` segment 中：
   ```bash
   curl -s --unix-socket /var/run/nft-forward.sock http://daemon/v1/ruleset
   # → {"owners":{"panel":[{...}]}}
   ```

7. 在另一个终端启动 TUI 并添加一条 rule：
   ```bash
   sudo nft-forward
   ```
   （TUI 会通过 unix socket 与 daemon 对话）

8. 再次查询 ruleset，应看到两个 owner 各自的 rules：
   ```bash
   curl -s --unix-socket /var/run/nft-forward.sock http://daemon/v1/ruleset
   # → {"owners":{"panel":[{...}],"tui":[{...}]}}
   ```

## Remote daemon (HTTP) role

此场景验证 daemon 监听 HTTP 端口并支持 Bearer token 认证，用于跨主机部署。

1. 在 panel 主机上启动 server：
   ```bash
   nft-forward server --addr :8080
   ```

2. 在第二个主机上启动 daemon 并配置 HTTP listen 和 token：
   ```bash
   # 创建 token 文件
   echo "my-secret-token" | sudo tee /etc/nft-forward/daemon.token
   
   # 启动 daemon 监听 HTTP
   sudo nft-forward daemon --listen :7878 --token-file /etc/nft-forward/daemon.token
   ```

3. 在 panel 中注册新节点：输入节点地址 `http://second-host:7878`，secret 设置为与 `daemon.token` 相同的值。

4. 在该节点上通过 panel 创建一条 forward。

5. 在第二个主机上验证 panel 创建的 forward 已推送到 daemon：
   ```bash
   curl -s -H "Authorization: Bearer my-secret-token" \
        http://localhost:7878/v1/ruleset
   # → {"owners":{"panel":[{...}]}}
   ```

## 已知限制

- **HTTP daemon 与 panel 已整合** — daemon 支持 HTTP listen 和 Bearer token 认证；server / TUI 皆已接入 daemon
- **Socket 权限** — unix socket 只有文件权限控制（生产部署只让 root + nft-forward group 用户能连）
- **自动化测试覆盖范围** — 当前所有自动化测试都在 `internal/daemon` 与 `internal/daemonclient` 单元层级覆盖；端到端 systemd / install.sh 流程仍依赖手工验证

## install / update / uninstall 验证

docker fixture 不跑 systemd，下列 case 需要真实 Linux 主机（VM / 物理机）。

### 1. 干净机器装 tui → update → 验证规则未丢

```bash
# 假设 release 已经发了 v1.2.3
sudo bash install.sh tui --release v1.2.3
sudo nft-forward          # 进 TUI，按 'a' 加一条转发规则（如 :10000 → 1.2.3.4:80）
# 退出 TUI；规则在 daemon state.json
```

发新版 v1.2.4，跑 update：

```bash
sudo bash install.sh update
sudo nft-forward          # 回 TUI 看刚才那条规则是否还在
```

预期：规则仍存在，新版二进制 sha 已经变化。检查 `journalctl -u nft-forward-daemon.service --since '5 minutes ago'` 看到正常启动日志。

### 2. agent 角色 update 保留 token

```bash
sudo bash install.sh agent --port 7878 --token deadbeef...64hex
ls -l /etc/nft-forward/daemon.token   # 记录 inode 和 mtime
sudo bash install.sh update
ls -l /etc/nft-forward/daemon.token   # 应当与上面一致，inode 不变
cat /etc/systemd/system/nft-forward-daemon.service | grep ExecStart
                                       # 应仍含 --listen :7878 --token-file ...
```

### 3. 角色切换链路 tui → server → tui

```bash
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
```

然后清旧 panel 段：

```bash
sudo bash install.sh uninstall server --purge
# server 已经没了；但 --purge 仍会调 daemon API 清 panel 段
sudo nft-forward          # 规则 A 仍在，规则 B 应已消失
```

### 4. daemon --purge 后从零重装

```bash
sudo bash install.sh uninstall server --purge 2>/dev/null || true
sudo bash install.sh uninstall agent  --purge 2>/dev/null || true
sudo bash install.sh uninstall daemon --purge
```

预期：

- `/var/lib/nft-forward/` 不存在
- `/etc/sysctl.d/99-nft-forward.conf` 不存在
- `nft list table ip nft_forward` 返回 `Error: No such file or directory`
- `getent group nft-forward` 无输出
- stderr 出现 tc qdisc 提示

然后从零起：

```bash
sudo bash install.sh tui
sudo nft-forward          # 应能空白起步
```

### 5. update 失败回滚（人为制造失败）

需要构造"sha 校验过但 daemon 起不来"的情况。最简单：用同一镜像但人为破坏 unit 的 ExecStart：

```bash
sudo bash install.sh tui
# 故意把 unit 改坏（指向不存在的 flag），让重启失败
sudo sed -i 's|/nft-forward daemon|/nft-forward daemon --bogus-flag|' \
    /etc/systemd/system/nft-forward-daemon.service
sudo systemctl daemon-reload
# 跑 update，新二进制启动时会因 --bogus-flag fail
sudo bash install.sh update
```

预期：

- 退出码 1
- stderr 打印 "update 失败，回滚到旧二进制"
- `/usr/local/sbin/nft-forward` 的 sha 与 update 前一致
- daemon 仍然起不来（unit 被改坏了）—— **这是单元损坏不是 update 单元错**；人工恢复 unit：

```bash
sudo sed -i 's| --bogus-flag||' /etc/systemd/system/nft-forward-daemon.service
sudo systemctl daemon-reload
sudo systemctl restart nft-forward-daemon.service
```

跑 update 才能验证完整回滚流；正式场景失败原因通常是新二进制 broken 而非 unit 错。
