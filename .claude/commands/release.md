# 发版部署

对 nft-forward 执行完整的发版 → GitHub Release → 远程部署流程。

## 前置检查

1. 确认当前在 `main` 分支且工作区干净
2. 运行 `go vet ./...` 和 `go test ./...`，全部通过才继续

## 确定版本号

1. 查看最近 tag：`git tag --sort=-v:refname | head -5`
2. 查看自上次 tag 以来的 commits：`git log <last-tag>..HEAD --oneline`
3. 根据变更类型决定版本号：
   - 新功能：minor bump（如 v0.9 → v0.10.0）
   - 修复/改进：patch bump（如 v0.10.0 → v0.10.1）
   - 小修补：micro bump（如 v0.10.1 → v0.10.1.1）

## 打 Tag 并推送

**必须先打 tag 再编译 nft-server**，否则 Go 的 VCS stamping 拿不到正确版本号（会生成 pseudo-version，如 v0.29.14 之后变成 v0.29.15）。

```bash
git tag -a vX.Y.Z -m "vX.Y.Z — 英文单行摘要"
git push origin main --tags
```

Tag message 用英文单行。

## 编译

先重建前端，nft-server 会 embed `web/dist`：

```bash
cd web && npm run build && cd ..
```

再交叉编译两个二进制：

```bash
# 面板：正常 buildinfo（版本号经 VCS stamping 进 nft-server）
# 必须在 git tag 之后执行，否则版本号不正确
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-server ./cmd/nft-server

# 节点：-buildvcs=false 让构建字节与 git 状态无关，相同源码跨次发布得到相同 sha
# （节点身份 = sha256，版本标签独立于二进制）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o build/nft-agent ./cmd/nft-agent
upx -9 --lzma build/nft-agent

(cd build && shasum -a 256 nft-server nft-agent > SHA256SUMS)
```

## 创建 GitHub Release

```bash
gh release create vX.Y.Z \
  --title "vX.Y.Z — 中文标题" \
  --notes "$(cat <<'EOF'
## 新增
- ...

## 修复
- ...

## 改进
- ...
EOF
)" --latest build/nft-server build/nft-agent build/SHA256SUMS install.sh
```

Release notes 用中文，分节（新增/修复/改进），与历史版本风格一致。assets 必须包含 `nft-server`（linux/amd64 ELF）、`nft-agent`（UPX --lzma 压缩的 linux/amd64 ELF）、`SHA256SUMS` 和 `install.sh`，否则 install.sh 的下载/校验会失败。

## 部署到服务器

```bash
/usr/bin/ssh hosthatch-jp "nft-forward-upgrade"
```

注意：必须用 `/usr/bin/ssh` 而非 `ssh`（环境有 wrapper 会导致失败）。

upgrade 脚本会自动：下载 latest → sha256 校验 → 备份 → 原子替换 → 重启 daemon + server → health-check。失败自动回滚。

## 验证

部署完成后确认输出中包含：
- `sha256: OK`
- `Update 完成`
- 新的 nft-agent sha256 与 SHA256SUMS 文件一致（server 机同时更新 nft-server）

## 约束

- nft-server 版本号经 buildinfo VCS stamping，不在源码中硬编码
- nft-agent 用 `-buildvcs=false` 可复现构建：版本号不进二进制，身份 = sha256，相同源码跨版本 sha 不变；版本标签由面板（推送时）与 install.sh（安装时写 `/etc/nft-forward/agent.version` + `agent.sha`）维护
- tag 必须用 annotated（`-a`），不要 lightweight tag
- release 必须标为 Latest（`--latest`），install.sh update 只拉 latest
- 不要在 commit message 或 release notes 中包含过程信息（任务编号、方案代号、审阅轮次）
