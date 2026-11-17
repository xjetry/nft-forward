package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// agentRepo / agentCacheRoot 定位面板要推送的 nft-agent。面板二进制里不含 agent：
// 它按自身的 release 版本从 GitHub release 取产物（每种节点架构一份，首次用到时
// 才取），缓存在磁盘上，再经 WS 链路发给节点，节点自己不需要访问 GitHub。
const (
	agentRepo      = "xjetry/nft-forward"
	agentCacheRoot = "/var/lib/nft-forward/agent-cache"
)

// agentAssets 是面板能推送的 nft-agent 发布资产：键是节点在 hello 里上报的
// GOARCH，值是该架构在 GitHub release 里的资产名。
//
// amd64 沿用无后缀名：已部署主机上持久化的升级脚本按 nft-agent 这个名字从
// latest release 取二进制，install.sh 也要能安装此前只发布了无后缀资产的旧
// release，所以这个名字不能挪作他用；其余架构一律带 -<arch> 后缀。release 里另有
// 内容相同的 nft-agent-amd64，是供按架构名手工取用的别名，面板不取它。
//
// 同一口径还有两处落点：install.sh 的「uname -m → 资产名」映射，以及
// scripts/build-release.sh 的构建矩阵。增删架构必须三处同改，否则会出现
// “release 里有产物但装不上/推不了”或反过来“面板去取一个不存在的资产”。
var agentAssets = map[string]string{
	"amd64": "nft-agent",
	"arm64": "nft-agent-arm64",
}

// errNodeNotConnected：升级只能推给当前挂在 hub 上的连接，节点离线时没有推送对象，
// 也无从得知它的架构。
var errNodeNotConnected = errors.New("节点未连接")

// agentAssetName 返回 arch 对应的发布资产名。表里没有的架构（含空串）一律报错
// 而不回落到某个默认产物：推过去的二进制节点跑不了时，带目标架构校验的 agent
// 会拒收，不带这项校验的 agent 会照单换上，重启后就起不来了。
func agentAssetName(arch string) (string, error) {
	asset, ok := agentAssets[arch]
	if !ok {
		return "", fmt.Errorf("节点架构 %q 没有对应的 nft-agent 发布产物，无法推送升级", arch)
	}
	return asset, nil
}

// agentArtifact 是面板会推给某一种架构节点的 nft-agent：面板自身 release 版本里
// 该架构的那份资产，连同它的 sha256。
type agentArtifact struct {
	Version string
	Arch    string
	SHA     string
	Data    []byte
}

// agentArtCache 按架构缓存已取到的产物；读写都持 agentArtMu。
var (
	agentArtMu    sync.Mutex
	agentArtCache = map[string]*agentArtifact{}
)

func serverVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return "dev"
	}
	return stripPseudoVersion(bi.Main.Version)
}

// stripPseudoVersion normalises a Go pseudo-version (e.g.
// v0.29.10-0.20260626041818-5dd657947da4) back to its base semver tag so the
// download URL hits an existing GitHub release. A clean tag passes through
// unchanged.
func stripPseudoVersion(v string) string {
	if i := strings.Index(v, "-0.2"); i > 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "+"); i > 0 {
		v = v[:i]
	}
	return v
}

// loadAgentArtifact 返回面板 release 版本里指定架构的 nft-agent，首次用到时拉取
// 并缓存。dev 构建的面板没有对应的 release，因此无法推送升级。
func (s *Server) loadAgentArtifact(arch string) (*agentArtifact, error) {
	asset, err := agentAssetName(arch)
	if err != nil {
		return nil, err
	}
	v := serverVersion()
	agentArtMu.Lock()
	defer agentArtMu.Unlock()
	// 缓存里已有当前版本的产物就不再下载（dev 构建也因此能推送预置进缓存的 agent）。
	if art := agentArtCache[arch]; art != nil && art.Version == v {
		return art, nil
	}
	if v == "dev" {
		return nil, errors.New("dev 构建无对应 agent release，无法推送升级")
	}
	data, sha, err := fetchAgentBinary(v, asset)
	if err != nil {
		return nil, err
	}
	art := &agentArtifact{Version: v, Arch: arch, SHA: sha, Data: data}
	agentArtCache[arch] = art
	return art, nil
}

// fetchAgentBinary 取 version 这个 release 里名为 asset（agentAssets 的某个值）的
// 资产：磁盘缓存里有就直接用，否则从 GitHub release 下载，对照该 release 的
// SHA256SUMS 校验通过后再写入缓存。缓存文件以资产名命名，各架构互不覆盖。
func fetchAgentBinary(version, asset string) ([]byte, string, error) {
	cacheBin := filepath.Join(agentCacheRoot, version, asset)
	if data, err := os.ReadFile(cacheBin); err == nil {
		sum := sha256.Sum256(data)
		return data, hex.EncodeToString(sum[:]), nil
	}
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", agentRepo, version)
	want, err := fetchSumFor(base+"/SHA256SUMS", asset)
	if err != nil {
		return nil, "", err
	}
	data, err := httpGetBytes(base + "/" + asset)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return nil, "", fmt.Errorf("%s sha256 校验失败: got %s want %s", asset, got, want)
	}
	if err := os.MkdirAll(filepath.Dir(cacheBin), 0o755); err == nil {
		_ = os.WriteFile(cacheBin, data, 0o755)
	}
	return data, got, nil
}

// fetchSumFor pulls the sha256 for name out of a SHA256SUMS file at url.
func fetchSumFor(url, name string) (string, error) {
	body, err := httpGetBytes(url)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中未找到 %s", name)
}

func httpGetBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// upgradeFor 构造发给节点的 Upgrade：节点按 sha 已在运行目标二进制时只同步版本
// 标签（Data 为空），否则是带内联二进制与 HTTP 回退地址的完整推送。art 必须是
// 该节点自身架构的产物（由 upgradeNode 保证）；回退地址里带上这个架构，
// serveBinary 才会返回同一份字节。
func upgradeFor(node *db.Node, art *agentArtifact, panelURL string) wsproto.Upgrade {
	if node.AgentSHA != "" && node.AgentSHA == art.SHA {
		return wsproto.Upgrade{Version: art.Version, SHA256: art.SHA}
	}
	return wsproto.Upgrade{
		Version: art.Version, SHA256: art.SHA,
		Size: int64(len(art.Data)), DownloadAt: panelURL + "/v1/binary?arch=" + art.Arch,
		Data: art.Data,
	}
}

// upgradeNode 把面板当前版本的 nft-agent 推给一个节点。
// 架构取自该节点当前 WS 连接的 hello（Hub.NodeArch），而不是库里的历史记录：
// 推送只能发给在线连接，而在线连接的 hello 就是“此刻那台机器上跑的是什么”的
// 第一手陈述；同一 token 换到另一种架构的机器上重装后，这里立刻跟着变。
func (s *Server) upgradeNode(node *db.Node, panelURL string) error {
	if err := requireMachine(node); err != nil {
		return err
	}
	arch, ok := s.Hub.NodeArch(node.ID)
	if !ok {
		return errNodeNotConnected
	}
	art, err := s.loadAgentArtifact(arch)
	if err != nil {
		return err
	}
	return s.Hub.SendUpgrade(node.ID, upgradeFor(node, art, panelURL))
}

// serveBinary 是 upgradeFor 给出的 HTTP 回退下载口。arch 必须显式给出：
// 这里没有“默认架构”可言，缺省时任选一个产物返回，等于让请求方装上一个可能
// 跑不了的二进制。
func (s *Server) serveBinary(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		http.Error(w, "缺少 arch 参数", http.StatusBadRequest)
		return
	}
	if _, err := agentAssetName(arch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	art, err := s.loadAgentArtifact(arch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(art.Data)))
	w.Header().Set("X-SHA256", art.SHA)
	w.Write(art.Data)
}

func (h *Hub) SendUpgrade(nodeID int64, u wsproto.Upgrade) error {
	h.mu.RLock()
	ac, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return errNodeNotConnected
	}

	id := ac.nextID()
	ch := make(chan json.RawMessage, 1)
	ac.pendMu.Lock()
	ac.pending[id] = ch
	ac.pendMu.Unlock()
	defer func() {
		ac.pendMu.Lock()
		delete(ac.pending, id)
		ac.pendMu.Unlock()
	}()

	payload, _ := json.Marshal(u)
	ac.enqueueWrite(wsproto.Envelope{Type: wsproto.TypeUpgrade, ID: id, Payload: payload})

	select {
	case raw := <-ch:
		var ack wsproto.UpgradeAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			return fmt.Errorf("malformed upgrade_ack: %w", err)
		}
		if !ack.OK {
			return fmt.Errorf("%s", ack.Error)
		}
		return nil
	case <-time.After(60 * time.Second):
		return fmt.Errorf("升级应答超时")
	case <-ac.closed:
		return fmt.Errorf("连接在升级期间断开")
	}
}
