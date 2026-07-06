package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"nft-forward/internal/db"
)

// itoa converts an int64 to its decimal string representation.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

func TestAPISetPerNodeQuota(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "qnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"traffic_quota_bytes": 1073741824})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/quota", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficQuotaBytes != 1073741824 {
		t.Fatalf("want 1073741824, got %d", g.TrafficQuotaBytes)
	}
}

func TestAPIResetPerNodeTraffic(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=traffic_used_bytes+? WHERE user_id=? AND node_id=?`, 5000, uid, n.ID)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/nodes/"+itoa(n.ID)+"/reset-traffic", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("want 0, got %d", g.TrafficUsedBytes)
	}
}

func TestAPIResetTrafficClearsPerNode(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "rtnode", "", "")
	db.GrantNode(d, uid, n.ID, 10, 0)
	d.Exec(`UPDATE users SET traffic_used_bytes=traffic_used_bytes+? WHERE id=?`, 3000, uid)
	d.Exec(`UPDATE user_nodes SET traffic_used_bytes=traffic_used_bytes+? WHERE user_id=? AND node_id=?`, 2000, uid, n.ID)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/reset-traffic", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficUsedBytes != 0 {
		t.Fatalf("global want 0, got %d", u.TrafficUsedBytes)
	}
	g, _ := db.GetNodeGrant(d, uid, n.ID)
	if g.TrafficUsedBytes != 0 {
		t.Fatalf("per-node want 0, got %d", g.TrafficUsedBytes)
	}
}

func TestAPISetResetDays(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	s, _ := New(d)
	adminCookie := loginAsAdmin(t, d)

	body, _ := json.Marshal(map[string]any{"traffic_reset_days": 30})
	req := httptest.NewRequest("POST", "/api/users/"+itoa(uid)+"/reset-days", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := db.GetUserByID(d, uid)
	if u.TrafficResetDays != 30 {
		t.Fatalf("want 30, got %d", u.TrafficResetDays)
	}
}
