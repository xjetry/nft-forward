# nft-forward 二进制拆分设计：nft-server / nft-agent

日期：2026-06-24
状态：已评审通过待实现

## 背景与目标

当前 `nft-forward` 是单一二进制，含面板（server）、守护进程（daemon/agent）、TUI 三种角色，约 12.7MB。节点侧（agent/tui）安装在大量边缘机上，体积越小越好。

经实测：`daemon` 仅为一个纯函数 `db.PickFreePort` 就 import 了 `internal/db`，把整个 `modernc.org/sqlite + libc`（约 3.4MB）拖进了节点二进制。断开这条依赖后，节点二进制 7.3MB（含 TUI），UPX --lzma 压到约 2.2MB。

目标：拆成两个产物，节点侧瘦身并可复现，面板按需把 agent 经 WS 推给节点（节点永不访问外网）。

## 决策（已与用户确认）

1. **两个产物**：`nft-server`（面板，含 web 前端 embed + sqlite + chi）、`nft-agent`（daemon + TUI 合一，无 sqlite/web）。`agent` 角色与 `tui` 角色都用 `nft-agent`，靠运行时 `--connect` 区分。
2. **面板取 agent 字节方式**：按需从 GitHub release 下载缓存；节点经 WS 收字节，永不访问 GitHub。
3. **压缩**：`nft-agent` 发版用 UPX --lzma 压缩；面板缓存/推送的也是该压缩版（单一 sha 同时用于 install 下载与 WS 推送）。
4. **版本号**：`nft-server` 与 `nft-agent` 共享同一 git tag、同次发布。
5. **版本-二进制解耦**：`nft-agent` 可复现构建（`-buildvcs=false -trimpath -ldflags="-s -w"`，版本号不进二进制）。二进制身份 = sha256，源码未变则跨版本 sha 不变；版本号是独立"标签"，由面板（推送时）与 install.sh（安装时）维护。
6. **推送目标版本**：绑定面板自身发布版本（下载与面板同 tag 的 nft-agent），保证 wsproto 协议兼容。

## 版本/sha 模型（核心）

- **nft-agent 身份 = sha256**：可复现构建保证"相同源码 → 相同字节 → 相同 sha"，与 git tag 无关。
- **版本号 = 标签**，存放在二进制之外：
  - 面板：`serverVersion()`（nft-server 走正常 buildinfo VCS stamping）= 发布 tag，即"面板要推的 agent 版本"。
  - 节点：在本地 state 持久化 `{version_label, agent_sha}`，hello/status 帧上报两者。
- **节点 sha 真值**：daemon 启动时对自身运行二进制自算 sha256 为准（避免 state 文件与实际二进制不一致）；version_label 取自 state。
- **首次安装**：install.sh 写入 `{version_label = RELEASE, agent_sha}` 到节点 state（**两者都写，不可遗漏**），保证节点首个 hello 即上报正确标识；daemon 启动以自算 sha 校正。

## 组件与改动

### 1. 代码拆分

- 新增 `internal/portutil`：移入 `PickFreePort` + `ChainPortMin/ChainPortMax`（纯函数，仅依赖 `math/rand`）。`internal/daemon/handlers.go` 改 import `portutil`。`internal/db` 中的 `PickFreePort`/常量改为薄包装转调 `portutil`（`db→portutil` 无环，仅 `math/rand`），server 端现有 `db.PickFreePort` 调用不动。关键不变量：`daemon` 不再 import `db`。
- `cmd/nft-server/main.go`：`runServer` / `runResetAdmin` / `bootstrap`（imports `db`、`server`）。
- `cmd/nft-agent/main.go`：`runDaemon` / `runTUI`（imports `daemon`、`daemonclient`、`tui`、`nft`、`sysdeps`、`portutil`）。无参 = TUI；`daemon …` = 守护。
- 删除 `cmd/nft-forward`。
- **验证不变量**：`go list -deps ./cmd/nft-agent` 不得出现 `modernc`、`sqlite`、`go-chi`、`internal/server`、`internal/db`。

### 2. wsproto

- `Hello` 增加 `AgentSHA string`（`AgentVersion` 已有）。如有 status/heartbeat 帧携带版本，也补 sha。
- `Upgrade` 复用现有字段（`Version`、`SHA256`、`Data`、`Size`、`DownloadAt`）。语义扩展：
  - `Data` 非空：正常推送二进制。
  - `Data` 为空且 `SHA256` == 节点自算 sha：**仅更新版本标签**（节点把 version_label 置为 `Version`，不写二进制，正常 ack）。

### 3. daemon（nft-agent 侧）

- 启动：自算运行二进制 sha256；读取 state 的 version_label；hello 上报 `{AgentVersion: label, AgentSHA: sha}`。
- 处理 Upgrade：
  - 有 `Data`：校验 sha → 替换二进制 → 持久化 `{version_label=Version, agent_sha=新sha}` → 重启。
  - 无 `Data`（标签同步）：校验自算 sha == `SHA256` → 仅持久化 `version_label=Version` → ack（不重启）。
- state 文件增加 `version_label`、`agent_sha` 字段。

### 4. server（nft-server 侧）

- **取 agent 字节**：新增"agent 缓存"模块。给定目标版本 `v`（= `serverVersion()`）：
  - 缓存命中（`/var/lib/nft-forward/agent-cache/<v>/nft-agent` + sha 记录）→ 直接用。
  - 未命中 → 从 `https://github.com/<repo>/releases/download/<v>/nft-agent` 下载、用同发布的 `SHA256SUMS` 校验 → 落盘缓存。
  - 面板离线/下载失败 → 推送返回清晰错误（约束只要求*节点*离线可用，面板假定有外网）。
  - dev 构建（`serverVersion()=="dev"`，无对应 release）→ 禁用推送与"非最新"提示。
- `loadSelfBinary` → 改为加载缓存的 nft-agent；`serveBinary`（HTTP 回退端点）吐缓存的 nft-agent。
- **推送决策**（替换现有 apiUpgrade / SendUpgrade 调用方）：
  1. 解析 target = `{version: v, sha: Y, data: bytes}`。
  2. 读节点当前 `{agent_version, agent_sha}`（来自最近 hello，存 DB）。
  3. `node.agent_sha == Y` → 发 `Upgrade{Version:v, SHA256:Y, Data:nil}`（仅同步标签）；DB 记 `agent_version=v`。
  4. 否则 → 发 `Upgrade{Version:v, SHA256:Y, Data:bytes}`；节点替换后重连上报新标识。

### 5. DB

- `nodes` 表增加 `agent_sha` 列（与 `agent_version` 并列）。`MarkNodeOnline` 同时存 version + sha。
- **⚠️ 加列三处对齐不变量**（见 memory `nodes-column-scan-lockstep`）：必须同改 `nodeCols`、`scanNode`、`grants.go` 的 inline scan（第三处隐蔽，漏了会静默清空授权节点列表）。

### 6. 版本检查 / API（point 5）

- 节点 API 用 **`agent_version`**（面板要推的 agent 版本 = `serverVersion()`）替代 `server_version`。可附带 target sha。
- "非最新"判定：`node.agent_sha != target.sha`（二进制与面板要推的不同）。面板可直接算出 `agent_outdated` 布尔 + target 版本标签返回，简化前端。
- 前端 `agentOutdated` 改用该字段；按钮文案"推送升级到 \<version\>"不变。

### 7. install.sh（point 3）

- `server` 角色：下载 **nft-server + nft-agent**（面板机 self-node daemon 需 nft-agent）。`nft-forward-server.service` 跑 nft-server；`nft-forward-daemon.service` 跑 `nft-agent daemon`。写 self-node 的 `{version, sha}`。
- `agent` 角色：仅下载 **nft-agent**，`daemon --connect`。写 `{version=RELEASE, sha}`。
- `tui` 角色：仅下载 **nft-agent**，`daemon`（无 connect）+ 用户跑 `nft-agent`。写 `{version=RELEASE, sha}`。
- 磁盘路径：`/usr/local/sbin/nft-server`、`/usr/local/sbin/nft-agent`。
- `nft-forward-upgrade`：按已装角色拉对应二进制并更新各自 `{version, sha}` 文件。
- **不可遗漏**：每条安装/升级路径都写 `{version, sha}`。

### 8. 发版流程（point 1，更新 release.md）

- 交叉编译 `nft-server`（正常 buildinfo）。
- 交叉编译 `nft-agent`（`-buildvcs=false -trimpath -ldflags="-s -w"`）→ UPX --lzma 压缩。
- `SHA256SUMS` 覆盖两个二进制。
- `gh release` 资产含 `nft-server`、`nft-agent`、`SHA256SUMS`、`install.sh`。
- 部署：`ssh hosthatch-jp nft-forward-upgrade`（拉两者、更新 self-node 标识）。

### 9. 节点一键安装命令（point 6）

- 面板节点详情页 installCmd 仍为 `install.sh agent --panel-url … --token …`；install.sh 的 agent 角色内部改为下载 nft-agent 并写 `{version, sha}`。命令文本基本不变。

## 错误处理

- 面板下载 agent 失败：推送 API 返回明确错误，不影响其它功能。
- 节点 sha 校验失败：拒绝替换，ack 带错误，保留旧二进制。
- 仅标签同步时节点自算 sha 与 `SHA256` 不一致：说明面板对节点状态判断过期（节点其实是别的二进制）→ 退化为正常推送（节点 ack 拒绝标签同步，面板下一轮带 Data 重推）。

## 测试

- `internal/portutil` 单测（端口选取）。
- `go list -deps ./cmd/nft-agent` 无重依赖（CI/脚本断言）。
- server：agent 缓存命中/未命中/校验失败；推送决策的"标签同步 vs 全量推送"分支。
- daemon：Upgrade 全量 vs 标签同步两条路径；启动自算 sha + 读 state 标签。
- wsproto：Hello 带 AgentSHA、Upgrade 空 Data 往返。
- DB：`agent_sha` 列三处对齐回归（授权节点列表不被清空）。

## 分期实现建议

- A 代码拆分（portutil + 两个 cmd + 可复现构建 + dep 断言）。
- B 版本/sha 模型（wsproto、daemon state、db 列、hello）。
- C 推送改造（agent 缓存下载、sha 跳过、标签同步）。
- D 版本检查 API + 前端。
- E install.sh + release.md + 节点安装命令。

## 非目标（YAGNI）

- 不做独立版本号、不做第三个产物（nft-tui）、不做多架构（保持 linux/amd64）。
- 不追 GitHub 最新 release（绑定面板自身版本）。
