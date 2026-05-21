package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newTestServer wires a Daemon with provided applier and a temp state file
// into an httptest.Server. Returns the daemon (for inspection) and the server.
func newTestServer(t *testing.T, applier Applier) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		applier:   applier,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		mu:        sync.Mutex{},
	}
	return d, httptest.NewServer(d.Handler())
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

func TestHandler_GetRulesetEmpty(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got rulesetPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 0 {
		t.Fatalf("expected empty rules, got %d", len(got.Rules))
	}
}

func TestHandler_PostRuleset_AppliesAndSavesAndIsReadable(t *testing.T) {
	fa := &fakeApplier{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":8080,"dest_ip":"1.2.3.4","dest_port":80}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if len(fa.last) != 1 || fa.last[0].SrcPort != 8080 {
		t.Fatalf("Apply not called with expected rule: %+v", fa.last)
	}
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 {
		t.Fatalf("state not saved, got %+v", saved)
	}

	// GET should now reflect the new rule.
	resp2, err := http.Get(srv.URL + "/v1/ruleset")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got rulesetPayload
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" {
		t.Fatalf("GET after POST mismatch: %+v", got.Rules)
	}
}

func TestHandler_PostRuleset_ApplyErrorReturns500AndDoesNotSave(t *testing.T) {
	fa := &fakeApplier{err: errors.New("nft failed")}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"rules":[{"id":"r1","proto":"tcp","src_port":1,"dest_ip":"1.2.3.4","dest_port":1}]}`
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	// state file should not have been created since apply failed.
	saved, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 0 {
		t.Fatalf("state was saved despite apply error: %+v", saved)
	}
	if len(d.rules) != 0 {
		t.Fatalf("d.rules mutated despite apply error: %+v", d.rules)
	}
}

func TestHandler_PostRuleset_BadJSON(t *testing.T) {
	_, srv := newTestServer(t, &fakeApplier{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/ruleset", "application/json", strings.NewReader("not json"))
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

