#!/usr/bin/env bash
# 构建一次发版的全部产物：每种架构一套 nft-server / nft-agent（amd64 的这一套同时
# 以不带后缀与带 -amd64 后缀两个名字发布），外加覆盖全部产物的 SHA256SUMS。产物
# 逐个做过 ELF 架构自检，每种架构的那一套在对应平台的容器里实际启动过一次，才算
# 构建成功。
#
# 用法: scripts/build-release.sh [vX.Y.Z]
#   vX.Y.Z   本次发版的 tag。给出时校验 nft-server 内嵌的版本戳与它完全一致；
#            省略时不做这项校验——只适合本地试构建，这样的产物不能拿去发版。
# 环境变量:
#   NFTF_BUILD_DIR     输出目录，默认 build
#   NFTF_SMOKE_IMAGE   试跑用的容器镜像，默认 busybox（须有 linux/amd64 与
#                      linux/arm64 两个变体；二进制是静态链接的，不依赖镜像内容）
#
# 依赖: go、npm（前端）、upx、docker（试跑；宿主须能运行两种架构的容器）。

set -euo pipefail
cd "$(dirname "$0")/.."

tag="${1:-}"
out="${NFTF_BUILD_DIR:-build}"
smoke_image="${NFTF_SMOKE_IMAGE:-busybox}"

die()  { echo "错误: $*" >&2; exit 1; }
note() { printf '\033[36m%s\033[0m\n' "$*"; }

# 本脚本里变量后面紧跟中文标点时一律写成 ${var}：macOS 自带的 bash 3.2 会把紧随
# 其后的多字节字符并进变量名，在 set -u 下报 unbound variable。

# 构建矩阵，每项是 GOARCH:该架构 ELF 头的 e_machine（十进制）:资产名后缀[:别名后缀]。
#
# 「资产名后缀」定下的名字，就是 install.sh 的「uname -m → 资产名」映射与面板推送
# 升级用的 internal/server/upgrade.go agentAssets 所取的名字：同一口径的三处落点，
# 增删架构必须三处同改。amd64 的这一栏为空：已部署主机上的升级脚本与更早的 release
# 都只认 nft-server / nft-agent 这两个名字。
#
# 「别名后缀」把同一份产物再以带架构名的名字发布一遍（amd64 → nft-server-amd64 /
# nft-agent-amd64），供按架构名手工取用；install.sh 与面板都不取别名。别名是那一份
# 产物的拷贝而不是另一次构建，并在校验步骤里逐字节比对：upgrade.go 的 upgradeFor
# 拿节点上报的 sha256 与 agentAssets 那个名字的 sha256 比较，相等才只同步版本标签。
# 两个名字的内容一旦不同，面板升级一个从别名装上的节点时就会把二进制再推一遍。
targets=(
  "amd64:62::-amd64"
  "arm64:183:-arm64"
)

for tool in go npm upx docker; do
  command -v "$tool" >/dev/null || die "缺少依赖: $tool"
done
if command -v sha256sum >/dev/null; then
  sha256=(sha256sum)
else
  sha256=(shasum -a 256)
fi

# elf_machine 输出文件 ELF 头里的 e_machine；不是 ELF 时返回非零。
# 只按小端读取：矩阵里的架构都是小端。
elf_machine() {
  local f="$1" magic
  magic="$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')"
  [[ "$magic" == "7f454c46" ]] || return 1
  # NF 过滤：BSD od 在数据行之后还会多输出一个空行。
  od -An -tu1 -j18 -N2 "$f" | awk 'NF { print $1 + $2 * 256; exit }'
}

# smoke 在 linux/$1 容器里以非 root 身份运行产物 $2（参数为 $4 起），要求输出里出现
# $3。判据是“程序自己打印的那句话出现了”而不是退出码：nft-agent 的这条路径本来
# 就以非零退出。
#
# 为什么必须真跑：upx -t 只验证压缩数据能还原，不验证解压桩能把程序拉起来。UPX
# 5.3.0 存在可执行段末尾贴近页边界时启动即 SIGTRAP 的缺陷（upx/upx#18967，x86_64
# 与 aarch64 都受影响），是否触发取决于每次构建的段布局，upx -t 对它一律报 OK。
# 面板推送升级时，节点换上新文件后直接重启，不像 install.sh update 那样带
# health-check 回滚，所以起不来的产物要在发版前拦下。
#
# 这一步证明的是“程序拉得起来”，不是“产物是该架构的”：宿主不是 $1 架构时容器
# 经模拟器运行，而装了多架构模拟器的宿主上，放错架构的二进制一样能跑。后者由
# 前面的 ELF 自检负责。
smoke() {
  local goarch="$1" name="$2" expect="$3"
  shift 3
  local log
  log="$(docker run --rm --platform "linux/$goarch" --user 65534:65534 \
           -v "$abs_out:/out:ro" "$smoke_image" "/out/$name" "$@" 2>&1)" || true
  if ! grep -qF -- "$expect" <<<"$log"; then
    echo "$log" >&2
    die "$name 在 linux/$goarch 上没有正常启动（输出中未见「${expect}」）"
  fi
  echo "  $name: 在 linux/$goarch 上启动正常"
}

note "[1/5] 构建前端 web/dist（nft-server 会把它 embed 进去）..."
(cd web && npm run build)

note "[2/5] 交叉编译 + 压缩 nft-agent ..."
mkdir -p "$out"
abs_out="$(cd "$out" && pwd)"
assets=()
for t in "${targets[@]}"; do
  IFS=: read -r goarch machine suffix alias_suffix <<<"$t"
  server="nft-server$suffix"
  agent="nft-agent$suffix"
  # 从干净状态开始，不在上一次构建留下的文件上原地覆盖：留在原地的 nft-agent 是
  # UPX 压过的，对它再压一次会报 AlreadyPackedException。
  rm -f "$out/$server" "$out/$agent"

  # 面板：保留 VCS stamping，版本号由此进入 nft-server（必须在打 tag 之后构建）。
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -ldflags="-s -w" -o "$out/$server" ./cmd/nft-server
  # 节点：-buildvcs=false 让产物字节与 git 状态无关，相同源码跨次发布得到相同
  # sha256（节点身份 = sha256，版本标签独立于二进制）。
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$out/$agent" ./cmd/nft-agent
  upx -q -9 --lzma "$out/$agent" >/dev/null
  assets+=("$server" "$agent")
  # 别名在压缩之后才拷贝：两个名字发布的必须是同一份字节（理由见构建矩阵的注释）。
  if [[ -n "$alias_suffix" ]]; then
    for base in nft-server nft-agent; do
      rm -f "$out/$base$alias_suffix"
      cp "$out/$base$suffix" "$out/$base$alias_suffix"
      assets+=("$base$alias_suffix")
    done
  fi
done

note "[3/5] 校验 ELF 目标架构、别名一致性与版本戳 ..."
for t in "${targets[@]}"; do
  IFS=: read -r goarch machine suffix alias_suffix <<<"$t"
  names=("nft-server$suffix" "nft-agent$suffix")
  if [[ -n "$alias_suffix" ]]; then
    names+=("nft-server$alias_suffix" "nft-agent$alias_suffix")
  fi
  for name in "${names[@]}"; do
    got="$(elf_machine "$out/$name")" || die "$name 不是 ELF 可执行文件"
    [[ "$got" == "$machine" ]] \
      || die "$name 的 e_machine=${got}，与 $goarch 应有的 $machine 不符（资产名与架构对不上）"
    echo "  $name: e_machine=$got ($goarch)"
  done
  if [[ -n "$alias_suffix" ]]; then
    for base in nft-server nft-agent; do
      cmp -s "$out/$base$suffix" "$out/$base$alias_suffix" \
        || die "${base}${alias_suffix} 与 ${base}${suffix} 的内容不同：别名必须是同一份产物的拷贝"
      echo "  $base$alias_suffix: 与 $base$suffix 内容一致"
    done
  fi

  stamp="$(go version -m "$out/nft-server$suffix" | awk '$1 == "mod" { print $3 }')"
  echo "  nft-server$suffix: 版本戳 $stamp"
  if [[ -n "$tag" && "$stamp" != "$tag" ]]; then
    die "nft-server$suffix 的版本戳是 ${stamp}，不是 ${tag}。常见原因：tag 没打在当前 HEAD 上（先 build 后 tag，或 checkout 落后于 tag）；工作区有未提交或未跟踪的文件（会带 +dirty）"
  fi
done
if [[ -z "$tag" ]]; then
  echo "  未指定 tag：没有校验版本戳，这批产物不能用于发版" >&2
fi

note "[4/5] 在目标架构上试跑 ..."
for t in "${targets[@]}"; do
  IFS=: read -r goarch machine suffix alias_suffix <<<"$t"
  # 期望文本取自各自的 main：nft-server 的 -h 用法里有 -addr 的说明；nft-agent 不带
  # 参数时进 TUI 路径，容器里没有 daemon socket，会报连不上 daemon。
  # 别名不单独试跑：它与这里跑的文件逐字节相同，上一步已校验。
  smoke "$goarch" "nft-server$suffix" "panel HTTP address" -h
  smoke "$goarch" "nft-agent$suffix" "无法连接 nft-forward daemon"
done

note "[5/5] 生成 SHA256SUMS ..."
(cd "$out" && rm -f SHA256SUMS && "${sha256[@]}" "${assets[@]}" > SHA256SUMS)
cat "$out/SHA256SUMS"

echo
note "产物（发版时连同 install.sh 一起上传）:"
for name in "${assets[@]}" SHA256SUMS; do
  printf '  %s/%s\n' "$out" "$name"
done
