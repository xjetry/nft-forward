package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nft-forward/internal/nft"
)

func newTestServer(t *testing.T, applier Applier) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		applier:   applier,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		mu:        sync.Mutex{},
		owners:    OwnerRuleset{},
	}
	return d, httptest.NewServer(d.Handler())
}

func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{
		applier:    &fakeApplier{},
		statePath:  filepath.Join(t.TempDir(), "state.json"),
		countersFn: defaultCounters,
		mu:         sync.Mutex{},
		owners:     OwnerRuleset{},
	}
	return d
}

func TestHandler_Health(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got["ok"] {
		t.Fatalf("expected ok=true, got %v", got)
	}
}

func TestHandler_GetRuleset_EmptyReturnsEmptyOwnersMap(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got fullPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Owners) != 0 {
		t.Fatalf("expected empty owners, got %+v", got.Owners)
	}
}

func TestHandler_PostOwnerSegment_AppliesAndSavesAndIsReadable(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(fa.last) != 1 || fa.last[0].SrcPort != 8080 {
		t.Fatalf("Apply not called with merged ruleset: %+v", fa.last)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved["tui"]) != 1 {
		t.Fatalf("state segment not saved: %+v", saved)
	}

	resp2, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got fullPayload
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Owners["tui"]) != 1 || got.Owners["tui"][0].ID != "r1" {
		t.Fatalf("GET after POST mismatch: %+v", got.Owners)
	}
}

func TestHandler_PostOwnerSegment_CrossOwnerPortConflictReturns409(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	resp1, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"t1","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("seed tui failed: %d", resp1.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/v1/ruleset/panel", "application/json",
		strings.NewReader(`{"rules":[{"id":"p1","proto":"tcp","src_port":80,"dest_ip":"2.0.0.0","dest_port":80}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp2.StatusCode)
	}
	if len(d.owners["tui"]) != 1 || len(d.owners["panel"]) != 0 {
		t.Fatalf("state mutated despite conflict: %+v", d.owners)
	}
}

func TestHandler_PostOwnerSegment_ApplyErrorReturns500AndDoesNotMutate(t *testing.T) {
	fa := &fakeApplier{err: errors.New("nft failed")}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"r1","proto":"tcp","src_port":1,"dest_ip":"1.0.0.0","dest_port":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("d.owners mutated despite apply error: %+v", d.owners)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("state was saved despite apply error: %+v", saved)
	}
}

func TestHandler_PostOwnerSegment_EmptyRulesClearsSegment(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[{"id":"x","proto":"tcp","src_port":80,"dest_ip":"1.0.0.0","dest_port":80}]}`))
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader(`{"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("empty rules POST should drop owner key, got: %+v", d.owners)
	}
}

func TestHandler_PostFlatRulesetReturns410Gone(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json",
		strings.NewReader(`{"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d", resp.StatusCode)
	}
}

func TestHandler_BadJSONOnOwnerEndpoint(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset/tui", "application/json",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_MissingOwnerInPathRejected(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset/", "application/json",
		strings.NewReader(`{"rules":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandler_PutRulesetNotAllowed(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/ruleset", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestHandleCounters(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]nft.Counter, error) {
		return []nft.Counter{{Proto: "tcp", ListenPort: 80, Bytes: 100, Packets: 2}}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got struct {
		Counters []nft.Counter `json:"counters"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Counters) != 1 || got.Counters[0].ListenPort != 80 {
		t.Errorf("unexpected counters: %+v", got.Counters)
	}
}

func TestHandleCounters_Error(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]nft.Counter, error) {
		return nil, fmt.Errorf("nft not available")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
