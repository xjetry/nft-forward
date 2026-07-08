package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAPIServedWithoutToken(t *testing.T) {
	d := openDB(t)
	s, _ := New(d)
	req := httptest.NewRequest("GET", "/api/v1/openapi.json", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi.json: want 200 without token, got %d", rec.Code)
	}
	var doc struct {
		OpenAPI string           `json:"openapi"`
		Paths   map[string]any   `json:"paths"`
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("openapi.json must be valid JSON: %v", err)
	}
	if doc.OpenAPI == "" || doc.Paths == nil {
		t.Fatalf("openapi doc missing openapi/paths keys: %s", rec.Body.String())
	}
	// base path lives in servers; the admin users endpoint is documented as /users
	if _, ok := doc.Paths["/users"]; !ok {
		t.Fatalf("openapi doc should document the /users endpoint, got paths: %v", doc.Paths)
	}
	if len(doc.Servers) == 0 || doc.Servers[0]["url"] != "/api/v1" {
		t.Fatalf("openapi servers must declare /api/v1 base, got %v", doc.Servers)
	}
}
