# 发版部署

对 nft-forward 执行完整的发版 → GitHub Release → 远程部署流程。

## 前置检查

1. 确认当前在 `main` 分支且工作区干净
2. 运行 `go vet ./...` 和 `go test ./...`，全部通过才继续

## 确定版本号

1. 查看最近 tag：`git tag --sort=-v:refname | head -5`
2. 查看自上次 tag 以来的 commits：`git log <last-tag>..HEAD --oneline`
3. 根据变更类型决定版本号（**严格三段 vX.Y.Z，禁止四段及以上**）：
   - 新功能：minor bump（如 v0.9.0 → v0.10.0）
   - 修复/改进/小修补：patch bump（如 v0.10.0 → v0.10.1）

## 打 Tag 并推送

**必须先打 tag 再编译 nft-server**，否则 Go 的 VCS stamping 拿不到正确版本号（会生成 pseudo-version，如 v0.29.14 之后变成 v0.29.15）。

```bash
git tag -a vX.Y.Z -m "vX.Y.Z — 英文单行摘要"
git push origin main --tags
```

Tag message 用英文单行。

## 编译

全部产物由一个脚本产出。**必须在 git tag 之后执行，并把 tag 传给它**：

```bash
scripts/build-release.sh vX.Y.Z
```

脚本依次做：重建前端（nft-server 会 embed `web/dist`）→ 为 amd64、arm64 各交叉编译 nft-server 与 nft-agent（nft-agent 再经 `upx -9 --lzma` 压缩）→ 把 amd64 的两个产物原样拷贝成带 `-amd64` 后缀的别名 → 校验每个产物的 ELF 架构与资产名对得上、别名与原文件内容一致 → 校验 nft-server 内嵌的版本戳等于传入的 tag → 在 docker 的 linux/amd64、linux/arm64 容器里把各自架构的 nft-server、nft-agent 实际启动一次 → 生成 `build/SHA256SUMS`。任何一步不过脚本都以非零退出，此时不要继续发版。

前提：本机 docker 在运行，且能跑 linux/amd64 与 linux/arm64 两种容器。

| 产物 | 架构 |
|------|------|
| `build/nft-server`、`build/nft-agent` | linux/amd64 |
| `build/nft-server-amd64`、`build/nft-agent-amd64` | linux/amd64，与上一行两个文件内容相同 |
| `build/nft-server-arm64`、`build/nft-agent-arm64` | linux/arm64 |
| `build/SHA256SUMS` | 覆盖以上六个 |

amd64 的资产名不带后缀：已部署主机上的升级脚本和更早的 release 都只认 `nft-server` / `nft-agent` 这两个名字。带 `-amd64` 后缀的两个是同一份文件的别名，供按架构名手工下载，install.sh 与面板都不取它们。增删架构要同时改三处——`scripts/build-release.sh` 的构建矩阵、`install.sh` 的架构映射、`internal/server/upgrade.go` 的 `agentAssets`。

版本戳校验不过（脚本会打印实际的版本戳）通常是下面几种情况之一：先 build 后 tag；本地 checkout 落后于 tag 所在的 commit；工作区有未提交或未跟踪的文件（版本戳会带 `+dirty`，未跟踪文件也算）。主工作区里有不便处理的未跟踪文件时，在 tag 上另开一个干净的 worktree 构建：`git worktree add --detach <目录> vX.Y.Z`，进去先 `(cd web && npm ci)`，再跑 `scripts/build-release.sh vX.Y.Z`，产物在该 worktree 的 `build/` 下。

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
)" --latest build/nft-server build/nft-agent build/nft-server-amd64 build/nft-agent-amd64 build/nft-server-arm64 build/nft-agent-arm64 build/SHA256SUMS install.sh
```

Release notes 用中文，分节（新增/修复/改进），与历史版本风格一致。assets 必须包含 `scripts/build-release.sh` 产出的六个二进制（`nft-server`、`nft-agent`、`nft-server-amd64`、`nft-agent-amd64`、`nft-server-arm64`、`nft-agent-arm64`）、`SHA256SUMS` 和 `install.sh`：缺不带后缀的两个，install.sh 与已部署的升级脚本会下载失败；缺 arm64 的两个，arm64 主机装不上，面板也无法向 arm64 节点推送升级。

## 部署到服务器

```bash
/usr/bin/ssh hosthatch-jp "nft-forward-upgrade update-script && nft-forward-upgrade"
```

先 `update-script` 再 update：失败时的回滚由主机上执行 update 的那份脚本完成，而主机上的 `nft-forward-upgrade` 是上一次保存的版本。`update-script` 取的是 main 分支的 `install.sh`，所以要在 main 推送之后执行。

注意：必须用 `/usr/bin/ssh` 而非 `ssh`（环境有 wrapper 会导致失败）。

upgrade 脚本会自动：下载 latest → sha256 校验 → 快照（二进制、daemon 单元、身份文件、`state.json`，server 机再加 `panel.db`，期间面板短暂停止）→ 原子替换 → 重启 daemon + server → health-check（daemon 与面板）。任何一步失败都自动回滚：快照里的文件全部还原后重新启动服务。

## 验证

部署完成后确认输出中包含：
- 每个下载的二进制各有一行 `sha256 OK: <资产名>`（主机上的升级脚本若还是旧版，这一行是 `<文件名>: OK`）
- `Update 完成`
- 新的 nft-agent sha256 与 SHA256SUMS 文件一致（server 机同时更新 nft-server）

## 约束

- nft-server 版本号经 buildinfo VCS stamping，不在源码中硬编码
- nft-agent 用 `-buildvcs=false` 可复现构建：版本号不进二进制，身份 = sha256，相同源码跨版本 sha 不变；版本标签由面板（推送时）与 install.sh（安装时写 `/etc/nft-forward/agent.version` + `agent.sha`）维护
- tag 必须用 annotated（`-a`），不要 lightweight tag
- release 必须标为 Latest（`--latest`），install.sh update 只拉 latest
- 不要在 commit message 或 release notes 中包含过程信息（任务编号、方案代号、审阅轮次）
