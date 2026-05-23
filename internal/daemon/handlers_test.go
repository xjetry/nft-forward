package daemon

import (
	"bytes"
	"context"
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

type fakeTcCall struct {
	rules []nft.Rule
	iface string
}

type fakeApplier struct {
	nftCalls     [][]nft.Rule
	tcCalls      []fakeTcCall
	err          error
	tcErr        error
	cleanupCalls int
}

func (f *fakeApplier) Apply(rules []nft.Rule, iface string) error {
	if f.err != nil {
		return f.err
	}
	f.nftCalls = append(f.nftCalls, append([]nft.Rule(nil), rules...))
	if f.tcErr != nil {
		return f.tcErr
	}
	f.tcCalls = append(f.tcCalls, fakeTcCall{
		rules: append([]nft.Rule(nil), rules...),
		iface: iface,
	})
	return nil
}

func (f *fakeApplier) Cleanup() error {
	f.cleanupCalls++
	return nil
}

func newTestServer(t *testing.T, applier Applier) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		applier:   applier,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		iface:     "eth0",
		resolveFn: func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
			// Default: passthrough resolver returns rules unchanged
			return rules, true, nil
		},
		mu:     sync.Mutex{},
		owners: OwnerRuleset{},
	}
	return d, httptest.NewServer(d.Handler())
}

func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{
		applier:   &fakeApplier{},
		statePath: filepath.Join(t.TempDir(), "state.json"),
		iface:     "eth0",
		countersFn: func() ([]nft.Counter, error) {
			panic("countersFn not injected — every test must set d.countersFn explicitly")
		},
		resolveFn: func(ctx context.Context, rules []nft.Rule) ([]nft.Rule, bool, error) {
			// Default: passthrough resolver returns rules unchanged
			return rules, true, nil
		},
		mu:     sync.Mutex{},
		owners: OwnerRuleset{},
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
	if len(fa.nftCalls) != 1 || fa.nftCalls[0][0].SrcPort != 8080 {
		t.Fatalf("Apply not called with merged ruleset: %+v", fa.nftCalls)
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

func TestHandleCounters_NilSliceEncodesAsEmptyArray(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]nft.Counter, error) { return nil, nil }

	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"counters":[]`) {
		t.Errorf("expected empty array in body, got %s", body)
	}
}

func TestApplyInvokesNftAndTcWithIface(t *testing.T) {
	fake := &fakeApplier{}
	d := newTestDaemon(t)
	d.applier = fake
	d.iface = "eth42"

	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80, BandwidthMbps: 50}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code/100 != 2 {
		t.Fatalf("status = %d", w.Code)
	}
	if len(fake.nftCalls) != 1 || len(fake.tcCalls) != 1 {
		t.Fatalf("expected one nft+tc call, got nft=%d tc=%d", len(fake.nftCalls), len(fake.tcCalls))
	}
	if fake.tcCalls[0].iface != "eth42" {
		t.Errorf("tc iface = %q, want eth42", fake.tcCalls[0].iface)
	}
}

func TestApply_TcFailure_StillReturnsErrorAfterNftRan(t *testing.T) {
	fake := &fakeApplier{tcErr: fmt.Errorf("tc broke")}
	d := newTestDaemon(t)
	d.applier = fake
	d.iface = "eth0"

	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if len(fake.nftCalls) != 1 {
		t.Errorf("nft should still have been invoked, got nftCalls=%d", len(fake.nftCalls))
	}
	if len(fake.tcCalls) != 0 {
		t.Errorf("tc should not have recorded a successful call, got tcCalls=%d", len(fake.tcCalls))
	}
}

func TestApplyResolvesDestHost(t *testing.T) {
	fake := &fakeApplier{}
	d := newTestDaemon(t)
	d.applier = fake
	// fake resolver: example.com -> 192.0.2.5
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		out := make([]nft.Rule, len(in))
		for i, r := range in {
			out[i] = r
			if r.DestHost == "example.com" {
				out[i].DestIP = "192.0.2.5"
			}
		}
		return out, true, nil
	}

	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestHost: "example.com", DestPort: 80}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code/100 != 2 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("expected 1 apply call")
	}
	got := fake.nftCalls[0][0]
	if got.DestIP != "192.0.2.5" {
		t.Errorf("DestIP = %q, want 192.0.2.5", got.DestIP)
	}
	// State persists raw rules so a refresh can re-resolve.
	state, err := LoadState(d.statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state["tui"][0].DestHost != "example.com" || state["tui"][0].DestIP != "" {
		t.Errorf("state should keep raw rule, got %+v", state["tui"][0])
	}
}

func TestApplyRejectsUnresolvableHost(t *testing.T) {
	d := newTestDaemon(t)
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		// resolver returns rules unchanged: DestHost still set, DestIP empty.
		return in, false, nil
	}
	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestHost: "nowhere.invalid", DestPort: 80}}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRefreshReAppliesWhenIPChanges(t *testing.T) {
	d := newTestDaemon(t)
	fake := d.applier.(*fakeApplier)

	// Seed an owner segment with a host-only rule.
	d.owners = OwnerRuleset{
		"tui": {{Proto: "tcp", SrcPort: 80, DestHost: "x.example.com", DestPort: 80}},
	}

	answer := "192.0.2.10"
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		out := make([]nft.Rule, len(in))
		changed := false
		for i, r := range in {
			out[i] = r
			if r.DestHost == "x.example.com" {
				if r.DestIP != answer {
					changed = true
				}
				out[i].DestIP = answer
			}
		}
		return out, changed, nil
	}

	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("first refresh should apply, got %d", len(fake.nftCalls))
	}

	// Second refresh with the same answer should be a no-op.
	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if len(fake.nftCalls) != 1 {
		t.Fatalf("idempotent refresh applied %d times", len(fake.nftCalls))
	}

	// IP changes -> apply again.
	answer = "192.0.2.11"
	if err := d.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refresh 3: %v", err)
	}
	if len(fake.nftCalls) != 2 {
		t.Fatalf("expected re-apply after IP change, got %d", len(fake.nftCalls))
	}
}

func TestRefreshAndHandlerNoRace(t *testing.T) {
	// Should pass under `go test -race`. Drives concurrent
	// handleRulesetOwner POST + refreshOnce calls to ensure no data race
	// on d.owners / d.lastResolved.
	d := newTestDaemon(t)
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		return in, false, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = d.refreshOnce(context.Background())
		}
	}()

	for i := 0; i < 50; i++ {
		rules := []nft.Rule{{Proto: "tcp", SrcPort: 80 + i, DestIP: "10.0.0.1", DestPort: 80}}
		body, _ := json.Marshal(map[string]any{"rules": rules})
		req := httptest.NewRequest(http.MethodPost, "/v1/ruleset/tui", bytes.NewReader(body))
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, req)
	}
	<-done
}

func TestHTTPListenerRequiresBearerToken(t *testing.T) {
	d := newTestDaemon(t)
	d.httpToken = "shhh"
	handler := d.httpHandler() // wraps Handler() with bearer middleware

	// Missing token.
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d", w.Code)
	}

	// Wrong token.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer nope")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d", w.Code)
	}

	// Right token.
	req = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer shhh")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("right token: status = %d", w.Code)
	}
}
