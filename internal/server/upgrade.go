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

// agentRepo / agentCacheRoot locate the nft-agent the panel pushes. Since the
// split the panel no longer contains the agent; it fetches the asset for its own
// release version from the GitHub release once and caches it on disk, then ships
// it to nodes over the WS link so the nodes never reach GitHub themselves.
const (
	agentRepo      = "xjetry/nft-forward"
	agentCacheRoot = "/var/lib/nft-forward/agent-cache"
)

// agentArtifact is the nft-agent binary the panel would push: the asset for the
// panel's own release version, with its sha256.
type agentArtifact struct {
	Version string
	SHA     string
	Data    []byte
}

var (
	agentArtMu    sync.Mutex
	agentArtCache *agentArtifact
)

func serverVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// loadAgentArtifact returns the nft-agent for the panel's release version,
// fetching+caching it on first use. A "dev" panel build has no matching release,
// so push/upgrade is unavailable there.
func (s *Server) loadAgentArtifact() (*agentArtifact, error) {
	v := serverVersion()
	agentArtMu.Lock()
	defer agentArtMu.Unlock()
	// A warmed cache for the current version short-circuits the download (and
	// lets a dev build push a pre-seeded agent).
	if agentArtCache != nil && agentArtCache.Version == v {
		return agentArtCache, nil
	}
	if v == "dev" {
		return nil, errors.New("dev 构建无对应 agent release，无法推送升级")
	}
	data, sha, err := fetchAgentBinary(v)
	if err != nil {
		return nil, err
	}
	agentArtCache = &agentArtifact{Version: v, SHA: sha, Data: data}
	return agentArtCache, nil
}

// fetchAgentBinary loads nft-agent for version from the on-disk cache, else
// downloads it from the GitHub release and verifies it against that release's
// SHA256SUMS before caching.
func fetchAgentBinary(version string) ([]byte, string, error) {
	cacheBin := filepath.Join(agentCacheRoot, version, "nft-agent")
	if data, err := os.ReadFile(cacheBin); err == nil {
		sum := sha256.Sum256(data)
		return data, hex.EncodeToString(sum[:]), nil
	}
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", agentRepo, version)
	want, err := fetchSumFor(base+"/SHA256SUMS", "nft-agent")
	if err != nil {
		return nil, "", err
	}
	data, err := httpGetBytes(base + "/nft-agent")
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if want != "" && got != want {
		return nil, "", fmt.Errorf("nft-agent sha256 校验失败: got %s want %s", got, want)
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

// upgradeFor builds the Upgrade to send a node: a label-only sync (empty Data)
// when the node already runs the target binary by sha, else the full payload
// with the inline binary and an HTTP fallback URL.
func upgradeFor(node *db.Node, art *agentArtifact, panelURL string) wsproto.Upgrade {
	if node.AgentSHA != "" && node.AgentSHA == art.SHA {
		return wsproto.Upgrade{Version: art.Version, SHA256: art.SHA}
	}
	return wsproto.Upgrade{
		Version: art.Version, SHA256: art.SHA,
		Size: int64(len(art.Data)), DownloadAt: panelURL + "/v1/binary",
		Data: art.Data,
	}
}

func (s *Server) serveBinary(w http.ResponseWriter, r *http.Request) {
	art, err := s.loadAgentArtifact()
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
		return fmt.Errorf("节点未连接")
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

