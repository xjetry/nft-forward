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

## 已知限制

- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI 已接入 daemon** — 运行 `sudo nft-forward`（默认子命令）会通过 unix socket 与 daemon 对话；daemon 没起会立即报错，不再 fallback 直接管 nftables
- **server / agent 仍走旧路径** — `nft-server` / `nft-agent` 二进制各自直接操作 nftables，与 daemon 并存会撞表。生产部署目前仍只能选一种：要么单机跑 daemon + TUI，要么跑 server/agent 集群（不要混用）
