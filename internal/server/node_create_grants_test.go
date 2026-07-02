package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// createNodeViaAPI posts a create-node body as admin and returns the new node ID.
func createNodeViaAPI(t *testing.T, s *Server, admin *http.Cookie, body map[string]any) int64 {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/nodes", bytes.NewReader(b))
	req.AddCookie(admin)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/nodes status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Node.ID
}

func TestCreateNodeGrantsUsers(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	u1, err := db.CreateUser(d, "alice", "x", "user")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := db.CreateUser(d, "bob", "x", "user")
	if err != nil {
		t.Fatal(err)
	}

	nodeID := createNodeViaAPI(t, s, admin, map[string]any{"name": "n1", "user_ids": []int64{u1, u2}})

	for _, uid := range []int64{u1, u2} {
		g, err := db.GetNodeGrant(d, uid, nodeID)
		if err != nil {
			t.Fatalf("grant for user %d missing: %v", uid, err)
		}
		if g.MaxForwards != defaultGrantMaxForwards {
			t.Fatalf("max_forwards = %d, want %d", g.MaxForwards, defaultGrantMaxForwards)
		}
	}
}

func TestCreateNodeWithoutUserIDsGrantsNothing(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	uid, err := db.CreateUser(d, "alice", "x", "user")
	if err != nil {
		t.Fatal(err)
	}

	nodeID := createNodeViaAPI(t, s, admin, map[string]any{"name": "n1"})

	if _, err := db.GetNodeGrant(d, uid, nodeID); err == nil {
		t.Fatalf("unexpected grant for user %d on node %d", uid, nodeID)
	}
}

func TestCreateCompositeNodeGrantsUsers(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	admin := loginAsAdmin(t, d)
	c1, _ := db.CreateNode(d, "c1", "", "t1")
	c2, _ := db.CreateNode(d, "c2", "", "t2")
	uid, err := db.CreateUser(d, "alice", "x", "user")
	if err != nil {
		t.Fatal(err)
	}

	nodeID := createNodeViaAPI(t, s, admin, map[string]any{
		"name": "combo", "node_type": "composite",
		"hops":     []map[string]any{{"node_id": c1.ID, "mode": "kernel"}, {"node_id": c2.ID, "mode": "userspace"}},
		"user_ids": []int64{uid},
	})

	if _, err := db.GetNodeGrant(d, uid, nodeID); err != nil {
		t.Fatalf("grant missing on composite node: %v", err)
	}
}
