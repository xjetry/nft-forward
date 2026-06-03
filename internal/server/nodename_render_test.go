package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// The admin forwards page renders the node column by name (resolved via the
// NodeByID map), falling back to #id only when the node row is absent. This
// guards against regressing to raw #id display.
func TestForwardsPageShowsNodeName(t *testing.T) {
	d := openDB(t)
	n, err := db.CreateNode(d, "po0-test", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateForward(d, &db.Forward{
		NodeID:     n.ID,
		Proto:      "tcp",
		ListenPort: 12345,
		TargetIP:   "10.0.0.9",
		TargetPort: 443,
		Mode:       "userspace",
	}); err != nil {
		t.Fatal(err)
	}

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/forwards", nil)
	req.AddCookie(loginAsAdmin(t, d))
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /forwards: status %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "po0-test") {
		t.Fatalf("node column should show the name 'po0-test', got body:\n%s", body)
	}
	if strings.Contains(body, fmt.Sprintf("#%d</td>", n.ID)) {
		t.Fatalf("node column still renders raw #%d", n.ID)
	}
}
