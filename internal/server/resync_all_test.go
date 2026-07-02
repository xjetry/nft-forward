package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// A composite node has no agent of its own, so dispatching to it always fails
// with "node N not connected". Before the fix, resync-all dispatched to every
// node including composites and stamped that failure into last_error — which
// the node-list page ignores for composites but the dashboard panel doesn't,
// producing two different status badges for the same node.
func TestResyncAllNodesSkipsComposite(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	req := httptest.NewRequest("POST", "/api/nodes/resync-all", nil)
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("resync-all status=%d body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetNode(d, comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError.Valid {
		t.Fatalf("composite node should never receive a dispatch error, got %q", got.LastError.String)
	}
}
