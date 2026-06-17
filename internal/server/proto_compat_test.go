package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

func TestCreateMyRuleAcceptsTCPUDP(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "edge", "https://p", "tok")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5)

	s, _ := New(d)
	for _, tc := range []struct {
		proto string
		want  int
	}{
		{"tcp+udp", http.StatusOK},
		{"udp", http.StatusOK},
		{"sctp", http.StatusBadRequest},
	} {
		body, _ := json.Marshal(map[string]any{
			"node_id": g.ID, "name": "r-" + tc.proto, "proto": tc.proto, "exit": "9.9.9.9:8443",
		})
		req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("proto %s: status=%d want=%d body=%s", tc.proto, rec.Code, tc.want, rec.Body.String())
		}
	}
}
