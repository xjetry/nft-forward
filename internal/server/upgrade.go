package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

var (
	selfBinaryOnce  sync.Once
	selfBinaryBytes []byte
	selfBinarySHA   string
	selfBinaryErr   error
)

func loadSelfBinary() {
	selfBinaryOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			selfBinaryErr = fmt.Errorf("os.Executable: %w", err)
			return
		}
		selfBinaryBytes, selfBinaryErr = os.ReadFile(exe)
		if selfBinaryErr != nil {
			return
		}
		h := sha256.Sum256(selfBinaryBytes)
		selfBinarySHA = hex.EncodeToString(h[:])
	})
}

func serverVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func (s *Server) serveBinary(w http.ResponseWriter, r *http.Request) {
	loadSelfBinary()
	if selfBinaryErr != nil {
		http.Error(w, selfBinaryErr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(selfBinaryBytes)))
	w.Header().Set("X-SHA256", selfBinarySHA)
	w.Write(selfBinaryBytes)
}

func (s *Server) upgradeNode(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	id, err := urlParamInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	loadSelfBinary()
	if selfBinaryErr != nil {
		s.flashRedirect(w, r, "无法读取 server 二进制: "+selfBinaryErr.Error(), fmt.Sprintf("/nodes/%d", id))
		return
	}

	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil || panelURL == "" {
		panelURL = "https://" + r.Host
	}
	downloadURL := panelURL + "/v1/binary"

	err = s.Hub.SendUpgrade(id, wsproto.Upgrade{
		Version:    serverVersion(),
		SHA256:     selfBinarySHA,
		Size:       int64(len(selfBinaryBytes)),
		DownloadAt: downloadURL,
	})
	if err != nil {
		s.flashRedirect(w, r, "推送升级失败: "+err.Error(), fmt.Sprintf("/nodes/%d", id))
		return
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade", strconv.FormatInt(id, 10), serverVersion())
	s.flashRedirect(w, r, "升级命令已推送，节点正在更新", fmt.Sprintf("/nodes/%d", id))
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

func (s *Server) upgradeAllNodes(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	loadSelfBinary()
	if selfBinaryErr != nil {
		s.flashRedirect(w, r, "无法读取 server 二进制: "+selfBinaryErr.Error(), "/nodes")
		return
	}
	panelURL, err := db.GetSetting(s.DB, "panel_url")
	if err != nil || panelURL == "" {
		panelURL = "https://" + r.Host
	}
	upgrade := wsproto.Upgrade{
		Version:    serverVersion(),
		SHA256:     selfBinarySHA,
		Size:       int64(len(selfBinaryBytes)),
		DownloadAt: panelURL + "/v1/binary",
	}
	nodes, err := db.ListNodes(s.DB)
	if err != nil {
		s.flashRedirect(w, r, err.Error(), "/nodes")
		return
	}
	var ok, fail int
	for _, n := range nodes {
		if n.AgentVersion == serverVersion() {
			continue
		}
		if err := s.Hub.SendUpgrade(n.ID, upgrade); err != nil {
			log.Printf("upgrade node %d (%s): %v", n.ID, n.Name, err)
			fail++
		} else {
			ok++
		}
	}
	db.WriteAudit(s.DB, u.ID, "node.upgrade_all", "", fmt.Sprintf("ok=%d fail=%d", ok, fail))
	s.flashRedirect(w, r, fmt.Sprintf("已推送升级：成功 %d，失败 %d", ok, fail), "/nodes")
}
