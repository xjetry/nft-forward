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

# 2. 取空 ruleset
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"rules":[]}

# 3. 提交一条 ruleset
curl -s --unix-socket /tmp/nft-forward.sock \
     -H 'Content-Type: application/json' \
     -X POST \
     -d '{"rules":[{"id":"rT","proto":"tcp","src_port":19090,"dest_ip":"127.0.0.1","dest_port":22}]}' \
     http://unix/v1/ruleset
# → {"count":1}

# 4. 验证内核确实写入了
sudo nft list table ip nft_forward | grep 19090
# → 应看到 'tcp dport 19090 counter ... dnat to 127.0.0.1:22'

# 5. 再 GET 一次
curl -s --unix-socket /tmp/nft-forward.sock http://unix/v1/ruleset
# → {"rules":[{"id":"rT",...}]}

# 6. 验证 state 文件
cat /tmp/nft-forward-state.json
# → 含 version: 1 + rules 数组

# 7. Ctrl-C 终止 daemon → /tmp/nft-forward.sock 应被清理
ls /tmp/nft-forward.sock
# → No such file or directory
```

## Restart 恢复验证

1. 重新启动 daemon（同样的 `--state` 路径）
2. 不发任何 POST，立即 `sudo nft list table ip nft_forward` — 应该看到上一次的 rule（Bootstrap 从 state.json 重放）

## 已知限制

- **无 owner segmentation** — 整套 ruleset 由后写的 POST 完全替换前一次提交。owner 分段是后续要做的能力
- **仅 unix socket** — 远程接入（HTTP + Bearer token）会在接入 server/agent 时再加
- **无认证** — 只有 socket 文件权限是访问控制（生产部署只让 root + nft-forward group 用户能连）
- **TUI / server / agent 仍走旧路径**，与 daemon 并存 — 同机同时跑 daemon 和旧 TUI/agent **会冲突**（都想独占本机 nftables 表），验证时只跑 daemon
