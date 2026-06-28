package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) apiSpeedWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()

	ctx := r.Context()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := s.Hub.speedCache.snapshot()
			data, err := json.Marshal(map[string]any{"speeds": snap})
			if err != nil {
				continue
			}
			if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
				return
			}
		}
	}
}
