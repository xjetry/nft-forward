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
