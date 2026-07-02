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
	"time"

	"nft-forward/internal/forward"
	"nft-forward/internal/nft"
)

type fakeDataplane struct {
	mu           sync.Mutex
	nftCalls     [][]nft.Rule // records each Reconcile's rule slice
	cleanupCalls int
	err          error             // Reconcile error
	counters     []forward.Counter // returned by Counters()
}

func (f *fakeDataplane) Reconcile(ctx context.Context, rules []nft.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nftCalls = append(f.nftCalls, append([]nft.Rule(nil), rules...))
	return f.err
}

func (f *fakeDataplane) Counters() ([]forward.Counter, error) { return f.counters, nil }

func (f *fakeDataplane) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanupCalls++
	return nil
}

func newTestServer(t *testing.T, dp Dataplane) (*Daemon, *httptest.Server) {
	t.Helper()
	d := &Daemon{
		dp:        dp,
		statePath: filepath.Join(t.TempDir(), "state.json"),
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
		dp:        &fakeDataplane{},
		statePath: filepath.Join(t.TempDir(), "state.json"),
		countersFn: func() ([]forward.Counter, error) {
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
	_, srv := newTestServer(t, &fakeDataplane{})
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

func TestHandler_GetStatus_Disconnected(t *testing.T) {
	_, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got statusResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Connected {
		t.Fatal("expected disconnected")
	}
}

func TestHandler_ListRules_EmptyReturnsEmptyArray(t *testing.T) {
	_, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/rules")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Rules []nft.Rule `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 0 {
		t.Fatalf("expected empty rules, got %+v", got.Rules)
	}
}

func TestHandler_ListRules_ReturnsTuiSegmentWhenDisconnected(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"tui":   {{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
		"panel": {{ID: "p1", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90}},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/rules", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var got struct {
		Rules []nft.Rule `json:"rules"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "t1" {
		t.Fatalf("expected tui rules, got %+v", got.Rules)
	}
}

func TestHandler_CreateRule_LocalAppliesAndSaves(t *testing.T) {
	fa := &fakeDataplane{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":80,"listen_port":12000}`
	resp, err := http.Post(srv.URL+"/v1/rules", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if len(fa.nftCalls) != 1 || fa.nftCalls[0][0].SrcPort != 12000 {
		t.Fatalf("Apply not called with correct rule: %+v", fa.nftCalls)
	}
	saved, _, err := LoadState(d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved["tui"]) != 1 {
		t.Fatalf("state segment not saved: %+v", saved)
	}
}

func TestHandler_CreateRule_AutoAssignsPort(t *testing.T) {
	fa := &fakeDataplane{}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":80}`
	resp, err := http.Post(srv.URL+"/v1/rules", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got createRuleResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ListenPort < 10001 || got.ListenPort > 20000 {
		t.Fatalf("auto-assigned port %d out of range", got.ListenPort)
	}
	if len(d.owners["tui"]) != 1 || d.owners["tui"][0].SrcPort != got.ListenPort {
		t.Fatalf("state not saved with auto-assigned port: %+v", d.owners["tui"])
	}
}

func TestHandler_CreateRule_ApplyErrorReturns500(t *testing.T) {
	fa := &fakeDataplane{err: errors.New("nft failed")}
	d, srv := newTestServer(t, fa)
	defer srv.Close()

	body := `{"proto":"tcp","exit_host":"1.0.0.0","exit_port":1,"listen_port":12000}`
	resp, err := http.Post(srv.URL+"/v1/rules", "application/json", strings.NewReader(body))
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
}

func TestHandler_UpdateRule_LocalHexID(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"tui": {{ID: "abcd1234", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	body := `{"comment":"updated","listen_port":90}`
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/abcd1234", strings.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if d.owners["tui"][0].Comment != "updated" || d.owners["tui"][0].SrcPort != 90 {
		t.Fatalf("rule not updated: %+v", d.owners["tui"][0])
	}
}

func TestHandler_UpdateRule_NotFound(t *testing.T) {
	d := newTestDaemon(t)
	body := `{"comment":"x"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/nope", strings.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
}

func TestHandler_UpdateRule_ServerIDReturns503WhenDisconnected(t *testing.T) {
	d := newTestDaemon(t)
	body := `{"listen_port":21000}`
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/5", strings.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandler_DeleteRule_LocalHexID(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"tui": {{ID: "abcd1234", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/rules/abcd1234", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("tui segment should be deleted after last rule removed: %+v", d.owners)
	}
}

func TestHandler_DeleteRule_NotFound(t *testing.T) {
	d := newTestDaemon(t)
	req := httptest.NewRequest(http.MethodDelete, "/v1/rules/nope", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
}

func TestHandler_DeleteRule_ServerIDReturns503WhenDisconnected(t *testing.T) {
	d := newTestDaemon(t)
	req := httptest.NewRequest(http.MethodDelete, "/v1/rules/9", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleCounters(t *testing.T) {
	d := newTestDaemon(t)
	d.countersFn = func() ([]forward.Counter, error) {
		return []forward.Counter{{Proto: "tcp", ListenPort: 80, BytesUp: 60, BytesDown: 40, Packets: 2}}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/counters", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got struct {
		Counters []forward.Counter `json:"counters"`
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
	d.countersFn = func() ([]forward.Counter, error) {
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
	d.countersFn = func() ([]forward.Counter, error) { return nil, nil }

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

func TestCreateRule_PassesResolvedRulesToReconcile(t *testing.T) {
	fake := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = fake

	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code/100 != 2 {
		t.Fatalf("status = %d", w.Code)
	}
	if len(fake.nftCalls) != 1 || len(fake.nftCalls[0]) != 1 {
		t.Fatalf("expected one Reconcile with one rule, got %+v", fake.nftCalls)
	}
}

func TestCreateRule_ReconcileErrorReturns500(t *testing.T) {
	fake := &fakeDataplane{err: fmt.Errorf("tc broke")}
	d := newTestDaemon(t)
	d.dp = fake

	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

func TestCreateRule_ResolvesDestHost(t *testing.T) {
	fake := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = fake
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

	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "example.com", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
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
	state, _, err := LoadState(d.statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state["tui"][0].DestHost != "example.com" || state["tui"][0].DestIP != "" {
		t.Errorf("state should keep raw rule, got %+v", state["tui"][0])
	}
}

func TestCreateRule_AcceptsUnresolvableButValidHost(t *testing.T) {
	fake := &fakeDataplane{}
	d := newTestDaemon(t)
	d.dp = fake
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		// nowhere.invalid 语法合法但解析不了：保持 DestIP 为空并报聚合错误
		return in, false, fmt.Errorf("dns: nowhere.invalid: no such host")
	}
	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "nowhere.invalid", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("status = %d, want 2xx (tolerant): %s", w.Code, w.Body.String())
	}
	// 规则入库（供刷新循环重试），但未下发到数据面（DestIP 空被跳过）
	if len(d.owners["tui"]) != 1 {
		t.Fatalf("rule should be stored in tui segment, got %+v", d.owners["tui"])
	}
	if len(fake.nftCalls) != 0 {
		t.Fatalf("unresolved rule must not reach dataplane, got %d apply calls", len(fake.nftCalls))
	}
}

func TestSetPanelRuleset_ReturnsWarningForUnresolved(t *testing.T) {
	d := newTestDaemon(t)
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		return in, false, fmt.Errorf("dns: bad.invalid: no such host")
	}
	warning, err := d.SetPanelRuleset(context.Background(), "rev1", []nft.Rule{
		{Proto: "tcp", SrcPort: 8080, DestHost: "bad.invalid", DestPort: 80},
	})
	if err != nil {
		t.Fatalf("SetPanelRuleset should not error on unresolved: %v", err)
	}
	if warning == "" {
		t.Fatal("expected non-empty warning for unresolved rule")
	}
}

func TestCreateRule_RejectsSyntacticallyInvalidHost(t *testing.T) {
	d := newTestDaemon(t)
	body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "4212", ExitPort: 80, ListenPort: 12000})
	req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for numeric host", w.Code)
	}
}

func TestRefreshReAppliesWhenIPChanges(t *testing.T) {
	d := newTestDaemon(t)
	fake := d.dp.(*fakeDataplane)

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
	// create rule + refreshOnce calls to ensure no data race
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
		body, _ := json.Marshal(createRuleReq{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 80, ListenPort: 12000 + i})
		req := httptest.NewRequest(http.MethodPost, "/v1/rules", bytes.NewReader(body))
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, req)
	}
	<-done
}

// concurrencyProbeDataplane records whether two Reconcile calls overlap,
// or whether a Close overlaps a Reconcile, so the reconcileMu serialization
// invariant can be asserted.
type concurrencyProbeDataplane struct {
	onReconcile func()
	onClose     func()
}

func (a *concurrencyProbeDataplane) Reconcile(ctx context.Context, rules []nft.Rule) error {
	if a.onReconcile != nil {
		a.onReconcile()
	}
	return nil
}

func (a *concurrencyProbeDataplane) Counters() ([]forward.Counter, error) { return nil, nil }

func (a *concurrencyProbeDataplane) Close(ctx context.Context) error {
	if a.onClose != nil {
		a.onClose()
	}
	return nil
}

func TestApplyIsSerializedAcrossRefreshAndWrite(t *testing.T) {
	dir := t.TempDir()
	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
	)
	fake := &concurrencyProbeDataplane{
		onReconcile: func() {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond) // widen the race window
			mu.Lock()
			inFlight--
			mu.Unlock()
		},
	}
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Dataplane:  fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.owners = OwnerRuleset{"tui": {{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.applySerialized(context.Background(), []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}})
		}()
	}
	wg.Wait()
	if maxInFlight > 1 {
		t.Fatalf("data plane reconciled concurrently: maxInFlight=%d", maxInFlight)
	}
}

func TestCleanupIsSerializedAgainstApply(t *testing.T) {
	dir := t.TempDir()
	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
	)
	track := func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	fake := &concurrencyProbeDataplane{onReconcile: track, onClose: track}
	d, err := New(Config{
		SocketPath: filepath.Join(dir, "s.sock"),
		StatePath:  filepath.Join(dir, "state.json"),
		Dataplane:  fake,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.applySerialized(context.Background(), []nft.Rule{{Proto: "tcp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80}})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = d.closeSerialized(context.Background())
	}()
	wg.Wait()
	if maxInFlight > 1 {
		t.Fatalf("close overlapped reconcile: maxInFlight=%d", maxInFlight)
	}
}

func TestCounterSamples_DeltasAndReset(t *testing.T) {
	d := &Daemon{dp: &fakeDataplane{counters: []forward.Counter{{Proto: "tcp", ListenPort: 80, BytesUp: 60, BytesDown: 40}}}}
	s1 := d.counterSamples()
	if len(s1) != 1 || s1[0].BytesUp != 60 || s1[0].BytesDown != 40 {
		t.Fatalf("first sample want up=60 down=40, got %+v", s1)
	}
	d.dp.(*fakeDataplane).counters = []forward.Counter{{Proto: "tcp", ListenPort: 80, BytesUp: 160, BytesDown: 90}}
	s2 := d.counterSamples()
	if len(s2) != 1 || s2[0].BytesUp != 100 || s2[0].BytesDown != 50 {
		t.Fatalf("second sample want up=100 down=50, got %+v", s2)
	}
	d.dp.(*fakeDataplane).counters = []forward.Counter{{Proto: "tcp", ListenPort: 80, BytesUp: 20, BytesDown: 10}} // reset
	s3 := d.counterSamples()
	if len(s3) != 1 || s3[0].BytesUp != 20 || s3[0].BytesDown != 10 {
		t.Fatalf("after reset want up=20 down=10, got %+v", s3)
	}
}

func TestParseRuleID(t *testing.T) {
	cases := []struct {
		in   string
		id   int64
		ok   bool
	}{
		{"5", 5, true},
		{"123", 123, true},
		{"0", 0, false},      // zero not valid
		{"", 0, false},       // empty
		{"abc", 0, false},    // hex
		{"abcd1234", 0, false}, // hex
		{"-1", 0, false},     // negative
	}
	for _, tc := range cases {
		got, ok := parseRuleID(tc.in)
		if ok != tc.ok || got != tc.id {
			t.Errorf("parseRuleID(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.id, tc.ok)
		}
	}
}

func TestPickLocalFreePort(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"tui": {{Proto: "tcp", SrcPort: 10001, DestIP: "1.0.0.0", DestPort: 80}},
	}
	port := d.pickLocalFreePort("tcp")
	if port == 0 {
		t.Fatal("no port available")
	}
	if port == 10001 {
		t.Fatal("picked occupied port")
	}
	if port < 10001 || port > 20000 {
		t.Fatalf("port %d out of range", port)
	}
}
