package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// makeComposite creates a composite node whose ordered children are childIDs.
func makeComposite(t *testing.T, d *sql.DB, name string, childIDs ...int64) *db.Node {
	t.Helper()
	c, err := db.CreateNode(d, name, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	hops := make([]db.NodeHop, len(childIDs))
	for i, cid := range childIDs {
		hops[i] = db.NodeHop{NodeID: c.ID, Position: i, HopNodeID: cid, Mode: "userspace"}
	}
	if err := db.CreateNodeHops(d, c.ID, hops); err != nil {
		t.Fatal(err)
	}
	n, _ := db.GetNode(d, c.ID)
	return n
}

func createMyRule(t *testing.T, s *Server, cookie *http.Cookie, nodeID int64, name string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"node_id": nodeID, "name": name, "proto": "tcp", "exit": "9.9.9.9:8443",
	})
	req := httptest.NewRequest("POST", "/api/my/rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// Granting a composite node authorizes the whole chain; the user need not be
// granted each sub-node separately.
func TestUserCreateRuleOnGrantedCompositeNode(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, comp.ID, 5) // only the composite, not its sub-nodes

	s, _ := New(d)
	rec := createMyRule(t, s, cookie, comp.ID, "vless")
	if rec.Code != http.StatusOK {
		t.Fatalf("composite grant should allow rule creation; status=%d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
}

// A composite node the user was NOT granted must still be rejected.
func TestUserCreateRuleRejectsUngrantedCompositeNode(t *testing.T) {
	d := openDB(t)
	a, _ := db.CreateNode(d, "hk", "", "")
	b, _ := db.CreateNode(d, "jp", "", "")
	_ = db.UpdateNodeRelayHost(d, a.ID, "1.1.1.1")
	_ = db.UpdateNodeRelayHost(d, b.ID, "2.2.2.2")
	comp := makeComposite(t, d, "chain", a.ID, b.ID)

	uid, cookie := loginAsUser(t, d, 10)
	// granted a sub-node but NOT the composite itself
	_ = db.GrantNode(d, uid, a.ID, 5)

	s, _ := New(d)
	rec := createMyRule(t, s, cookie, comp.ID, "vless")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted composite must be 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 0 {
		t.Fatalf("ungranted composite must create no rule; got %d", len(rules))
	}
}

// The my-rules list item must expose rule fields (id, node_id, name) at the top
// level, because the React list reads r.id / r.node_id / r.name flat. A wrapped
// {"rule":{...}} shape leaves them undefined and breaks delete (id=undefined).
func TestMyListRulesItemShapeIsFlat(t *testing.T) {
	d := openDB(t)
	g, _ := db.CreateNode(d, "node", "", "")
	_ = db.UpdateNodeRelayHost(d, g.ID, "1.1.1.1")
	uid, cookie := loginAsUser(t, d, 10)
	_ = db.GrantNode(d, uid, g.ID, 5)

	s, _ := New(d)
	if rec := createMyRule(t, s, cookie, g.ID, "r1"); rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}

	lreq := httptest.NewRequest("GET", "/api/my/rules", nil)
	lreq.AddCookie(cookie)
	lrec := httptest.NewRecorder()
	s.Router().ServeHTTP(lrec, lreq)
	if lrec.Code != http.StatusOK {
		t.Fatalf("list status=%d", lrec.Code)
	}
	var resp struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(resp.Rules))
	}
	item := resp.Rules[0]
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	idf, ok := item["id"].(float64)
	if !ok || int64(idf) == 0 {
		t.Fatalf("list item must have non-zero top-level id; got %v (keys=%v)", item["id"], keys)
	}
	if _, ok := item["node_id"]; !ok {
		t.Fatalf("list item missing top-level node_id (keys=%v)", keys)
	}
	if item["name"] != "r1" {
		t.Fatalf("list item name=%v, want r1 (keys=%v)", item["name"], keys)
	}
	// View fields must survive the flattening.
	if _, ok := item["path"]; !ok {
		t.Fatalf("list item missing path (keys=%v)", keys)
	}
}
