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
	"nft-forward/internal/wsproto"
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

// 规则列表覆盖数据面里的每一条规则，与此刻连没连着面板无关：面板下发的在前，本地的
// 在后。两种归属名下同时有规则的情形是真实存在的——接入面板时本地规则交给面板接管，被面板
// 拒绝的那一批留在本地名下继续转发；面板不可达时面板名下的规则也照常在转发。
func TestHandler_ListRules_CoversBothOwnersWhateverTheConnectionState(t *testing.T) {
	for _, connected := range []bool{false, true} {
		d := newTestDaemon(t)
		d.owners = OwnerRuleset{
			"tui":   {{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
			"panel": {{ID: "p1", RuleID: 7, Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90}},
		}
		if connected {
			dl := NewDialer(DialerConfig{})
			dl.connected.Store(true)
			d.dialer.Store(dl)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/rules", nil)
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("connected=%v status=%d", connected, w.Code)
		}
		var got struct {
			Rules []nft.Rule `json:"rules"`
		}
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Rules) != 2 || got.Rules[0].ID != "p1" || got.Rules[1].ID != "t1" {
			t.Fatalf("connected=%v：应列出面板规则 p1 与本地规则 t1，实际 %+v", connected, got.Rules)
		}
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
		t.Fatalf("state not saved: %+v", saved)
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
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/local/abcd1234", strings.NewReader(body))
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
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/local/nope", strings.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
	}
}

func TestHandler_DeleteRule_LocalHexID(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"tui": {{ID: "abcd1234", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/rules/local/abcd1234", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	if _, exists := d.owners["tui"]; exists {
		t.Fatalf("the local owner's key should be gone after its last rule is removed: %+v", d.owners)
	}
}

func TestHandler_DeleteRule_NotFound(t *testing.T) {
	d := newTestDaemon(t)
	req := httptest.NewRequest(http.MethodDelete, "/v1/rules/local/nope", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", w.Code)
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
		t.Fatalf("rule should be stored under the local owner, got %+v", d.owners["tui"])
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

// 限速分组的 id 超过 16 位时内核数据面建不出 class，这样的规则不限速。daemon 应用
// 面板规则集时把它们写进告警（面板把告警显示在节点上），不悄悄放过。分组放得下的
// 规则、用户态转发的规则、不限速的规则都不在告警里；没有这样的规则时没有告警。
func TestSetPanelRulesetWarnsAboutGroupsTheKernelCannotShape(t *testing.T) {
	d := newTestDaemon(t)
	fits := nft.Rule{Proto: "tcp", SrcPort: 12001, DestIP: "10.0.0.1", DestPort: 80, ShapeGroup: 5, RateMBytes: 10}
	oversize := nft.Rule{Proto: "tcp", SrcPort: 12002, DestIP: "10.0.0.1", DestPort: 80, ShapeGroup: 0x10000, RateMBytes: 10}
	oversizeUserspace := nft.Rule{Proto: "tcp", SrcPort: 12003, DestIP: "10.0.0.1", DestPort: 80, ShapeGroup: 0x10000, RateMBytes: 10, Mode: nft.ModeUserspace}
	unshaped := nft.Rule{Proto: "tcp", SrcPort: 12004, DestIP: "10.0.0.1", DestPort: 80}

	warning, err := d.SetPanelRuleset(context.Background(), "rev1", []nft.Rule{fits, oversize, oversizeUserspace, unshaped})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "12002") || !strings.Contains(warning, "限速") {
		t.Errorf("告警应点名端口 12002 的规则不限速，实际 %q", warning)
	}
	for _, port := range []string{"12001", "12003", "12004"} {
		if strings.Contains(warning, port) {
			t.Errorf("端口 %s 的规则不该出现在告警里，实际 %q", port, warning)
		}
	}

	warning, err = d.SetPanelRuleset(context.Background(), "rev2", []nft.Rule{fits, oversizeUserspace, unshaped})
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("没有内核数据面限不了速的规则时不应有告警，实际 %q", warning)
	}

	// 与“目标无法解析”的告警同时出现时两条都在。
	d.resolveFn = func(ctx context.Context, in []nft.Rule) ([]nft.Rule, bool, error) {
		return in, false, fmt.Errorf("dns: bad.invalid: no such host")
	}
	warning, err = d.SetPanelRuleset(context.Background(), "rev3", []nft.Rule{
		oversize,
		{Proto: "tcp", SrcPort: 12005, DestHost: "bad.invalid", DestPort: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "12002") || !strings.Contains(warning, "bad.invalid") {
		t.Errorf("两条告警都应在，实际 %q", warning)
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

	// Seed one owner's rules with a host-only rule.
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

func TestPanelHopCount(t *testing.T) {
	d := &Daemon{owners: OwnerRuleset{
		"panel": {
			{ID: "a1", RuleID: 5, HopCount: 3},
			{ID: "a2", RuleID: 6, HopCount: 1},
		},
		"tui": {{ID: "b1"}},
	}}
	if got := d.panelHopCount(5); got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
	if got := d.panelHopCount(6); got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
	if got := d.panelHopCount(99); got != 0 {
		t.Fatalf("unknown rule want 0, got %d", got)
	}
}

func TestUpdateRuleRoutesChainRowsToRuleHopEdit(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	ok := func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	}
	fh.onAck(wsproto.TypeRuleHopEdit, ok)
	fh.onAck(wsproto.TypeRuleUpdate, ok)
	hubSrv := httptest.NewServer(fh.handler(t))
	defer hubSrv.Close()

	d, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	panel := RemotePanel("ws"+strings.TrimPrefix(hubSrv.URL, "http")+"/", "tok")
	d.mode = panel
	dl := NewDialer(DialerConfig{
		Panel: panel, AgentVersion: "v1",
		GetState: func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:  func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()
	waitConnected(t, dl)
	d.dialer.Store(dl)
	d.mu.Lock()
	d.owners = OwnerRuleset{"panel": {
		{ID: "x1", RuleID: 5, HopCount: 3},
		{ID: "x2", RuleID: 6, HopCount: 1},
	}}
	d.mu.Unlock()

	put := func(id, body string) int {
		req := httptest.NewRequest(http.MethodPut, "/v1/rules/panel/"+id, strings.NewReader(body))
		w := httptest.NewRecorder()
		d.handleRulesWithID(w, req)
		return w.Code
	}
	if code := put("5", `{"listen_port":12345,"mode":"userspace","comment":"c"}`); code != http.StatusOK {
		t.Fatalf("chain-row edit: want 200, got %d", code)
	}
	if code := put("6", `{"proto":"tcp","exit_host":"9.9.9.9","exit_port":443}`); code != http.StatusOK {
		t.Fatalf("single-hop edit: want 200, got %d", code)
	}
	var sawHopEdit, sawUpdate bool
	for _, f := range fh.Frames() {
		switch f.Type {
		case wsproto.TypeRuleHopEdit:
			sawHopEdit = true
			var e wsproto.RuleHopEdit
			_ = json.Unmarshal(f.Payload, &e)
			if e.RuleID != 5 || e.ListenPort != 12345 || e.Mode != "userspace" {
				t.Fatalf("rule_hop_edit payload wrong: %+v", e)
			}
		case wsproto.TypeRuleUpdate:
			sawUpdate = true
		}
	}
	if !sawHopEdit || !sawUpdate {
		t.Fatalf("want both channels used, hopEdit=%v update=%v", sawHopEdit, sawUpdate)
	}
}
