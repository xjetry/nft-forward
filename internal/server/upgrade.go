package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

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

