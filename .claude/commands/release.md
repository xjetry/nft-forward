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

## 编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o build/nft-forward ./cmd/nft-forward
(cd build && shasum -a 256 nft-forward > SHA256SUMS)
```

## 打 Tag 并推送

```bash
git tag -a vX.Y.Z -m "vX.Y.Z — 英文单行摘要"
git push origin main --tags
```

Tag message 用英文单行。

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
)" --latest build/nft-forward build/SHA256SUMS
```

Release notes 用中文，分节（新增/修复/改进），与历史版本风格一致。assets 必须包含 `nft-forward`（linux/amd64 ELF）和 `SHA256SUMS`，否则 install.sh 的下载/校验会失败。

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
- 新的二进制 sha256 与 SHA256SUMS 文件一致

## 约束

- 版本号不在源码中注入（`agentVersion()` 通过 `debug.ReadBuildInfo()` 读取）
- tag 必须用 annotated（`-a`），不要 lightweight tag
- release 必须标为 Latest（`--latest`），install.sh update 只拉 latest
- 不要在 commit message 或 release notes 中包含过程信息（任务编号、方案代号、审阅轮次）
