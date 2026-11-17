package daemonclient

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// shortSockDir mirrors internal/daemon's helper: macOS sun_path is capped
// at 104 bytes and t.TempDir under /var/folders is often too long.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nftc-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// mockServer starts an HTTP server bound to a unix socket inside a short
// temp dir and serves handlerFn. Returns the socket path so the test can
// build a Client. The server is closed automatically via t.Cleanup.
func mockServer(t *testing.T, handlerFn http.HandlerFunc) string {
	t.Helper()
	sockPath := filepath.Join(shortSockDir(t), "mock.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handlerFn}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); l.Close() })
	return sockPath
}

func TestHealth_OK(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestHealth_FailsWhenSocketMissing(t *testing.T) {
	c, err := New(filepath.Join(shortSockDir(t), "nope.sock"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err == nil {
		t.Fatal("expected error when socket does not exist")
	}
}

func TestHealth_FailsOnNon200(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.Health()
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}
}

func TestHealth_FailsOnOkFalse(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.Health()
	if err == nil || !strings.Contains(err.Error(), "ok=false") {
		t.Fatalf("expected ok=false error, got %v", err)
	}
}

// respondWith 起一个只回固定应答体的假 daemon，返回连到它的客户端。
func respondWith(t *testing.T, body string) *Client {
	t.Helper()
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestStatus_ReportsModeAndSession(t *testing.T) {
	s, err := respondWith(t, `{"mode":"remote-panel","connected":true,"node_name":"edge","node_id":7}`).Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Mode != ModeRemotePanel || !s.Mode.Managed() || !s.Connected || s.NodeName != "edge" || s.NodeID != 7 {
		t.Fatalf("unexpected status: %+v", s)
	}

	s, err = respondWith(t, `{"mode":"local-panel","connected":false}`).Status()
	if err != nil || s.Mode != ModeLocalPanel || !s.Mode.Managed() || s.Connected {
		t.Fatalf("面板机本机、离线：得到 %+v, %v", s, err)
	}

	s, err = respondWith(t, `{"mode":"standalone","connected":false}`).Status()
	if err != nil || s.Mode != ModeStandalone || s.Mode.Managed() {
		t.Fatalf("单机：得到 %+v, %v", s, err)
	}
}

// 形态认不出时报错，不猜成某一种。缺了 mode 的应答（比本程序旧的 daemon 就是这样
// 回的）也走这条路：把它当成单机，就是把托管但离线的 daemon 说成本地模式。
func TestStatus_RefusesAnUnrecognisedMode(t *testing.T) {
	for _, body := range []string{
		`{"connected":true,"node_name":"edge","node_id":7}`,
		`{"mode":"","connected":false}`,
		`{"mode":"clustered","connected":false}`,
	} {
		if s, err := respondWith(t, body).Status(); err == nil || !strings.Contains(err.Error(), "无法识别的运行形态") {
			t.Errorf("应答 %s：应报形态认不出，实际 %+v, %v", body, s, err)
		}
	}
}

func TestListRules_RowsCarryTheDaemonsAddress(t *testing.T) {
	got, err := respondWith(t, `{"rules":[
		{"proto":"tcp","src_port":90,"dest_ip":"2.0.0.0","dest_port":90,"rule_id":12345678,"ref":{"ns":"panel","id":"12345678"}},
		{"id":"12345678","proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80,"ref":{"ns":"local","id":"12345678"}}
	]}`).ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected rows: %+v", got)
	}
	if got[0].SrcPort != 90 || got[0].Ref != PanelRule(12345678) || got[0].Ref.Local() {
		t.Errorf("第一行应是面板规则 panel/12345678，实际 %+v", got[0])
	}
	if got[1].SrcPort != 80 || got[1].Ref != LocalRule("12345678") || !got[1].Ref.Local() {
		t.Errorf("第二行应是本地规则 local/12345678，实际 %+v", got[1])
	}
}

// 行的地址只认 daemon 给的。没给地址（比本程序旧的 daemon）或地址不成形时整个列表
// 作废，不拿规则的字段去补一个出来。
func TestListRules_RefusesRowsWithoutAUsableAddress(t *testing.T) {
	for name, body := range map[string]string{
		"没有地址":   `{"rules":[{"id":"r1","proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80}]}`,
		"未知命名空间": `{"rules":[{"id":"r1","proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80,"ref":{"ns":"tui","id":"r1"}}]}`,
		"id 为空":  `{"rules":[{"proto":"tcp","src_port":80,"dest_ip":"1.2.3.4","dest_port":80,"ref":{"ns":"local","id":""}}]}`,
	} {
		if rows, err := respondWith(t, body).ListRules(); err == nil {
			t.Errorf("%s：应报错，实际返回 %+v", name, rows)
		}
	}
}

func TestListRules_EmptyReturnsNonNilSlice(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rules":[]}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("expected zero rules, got %+v", got)
	}
}

func TestCreateRule_SendsBodyAndReturnsEntry(t *testing.T) {
	var capturedBody []byte
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entry":"10.0.0.1:12000","listen_port":12000}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.CreateRule(CreateRuleReq{
		Proto: "tcp", ExitHost: "1.2.3.4", ExitPort: 80, ListenPort: 12000,
	})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if resp.ListenPort != 12000 {
		t.Errorf("ListenPort = %d, want 12000", resp.ListenPort)
	}
	var got CreateRuleReq
	if err := json.Unmarshal(capturedBody, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Proto != "tcp" || got.ExitHost != "1.2.3.4" {
		t.Fatalf("body mismatch: %+v", got)
	}
}

func TestCreateRule_PropagatesServerError(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "port conflict", http.StatusConflict)
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.CreateRule(CreateRuleReq{Proto: "tcp", ExitHost: "1.0.0.0", ExitPort: 80})
	if err == nil || !strings.Contains(err.Error(), "port conflict") {
		t.Fatalf("expected error with port conflict, got %v", err)
	}
}

func TestUpdateRule_PutsWithID(t *testing.T) {
	var capturedMethod, capturedPath string
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.UpdateRule(LocalRule("abc123"), UpdateRuleReq{Proto: "tcp", ExitHost: "1.2.3.4", ExitPort: 80})
	if err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	if capturedMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", capturedMethod)
	}
	if capturedPath != "/v1/rules/local/abc123" {
		t.Errorf("path = %q, want /v1/rules/local/abc123", capturedPath)
	}
}

// 地址里的命名空间只取决于用哪个构造函数，与 id 的长相无关：全是数字的本地 id
// 仍落在 local 下，面板规则落在 panel 下。
func TestRuleRefPathCarriesNamespace(t *testing.T) {
	cases := []struct {
		ref  RuleRef
		want string
	}{
		{LocalRule("abcd1234"), "/v1/rules/local/abcd1234"},
		{LocalRule("12345678"), "/v1/rules/local/12345678"},
		{PanelRule(7), "/v1/rules/panel/7"},
		{PanelRule(12345678), "/v1/rules/panel/12345678"},
	}
	for _, tc := range cases {
		if got := tc.ref.path(); got != tc.want {
			t.Errorf("%s: path = %q, want %q", tc.ref, got, tc.want)
		}
	}
	if LocalRule("12345678") == PanelRule(12345678) {
		t.Error("同一串数字在两个命名空间里必须是两个不同的地址")
	}
}

func TestUpdateRule_SurfacesServerError(t *testing.T) {
	sock := mockServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "端口被占用", http.StatusBadRequest)
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.UpdateRule(PanelRule(5), UpdateRuleReq{ListenPort: 21000})
	if err == nil || !strings.Contains(err.Error(), "端口被占用") {
		t.Fatalf("expected server error surfaced verbatim, got %v", err)
	}
}

func TestDeleteRule_DeletesWithID(t *testing.T) {
	var capturedMethod, capturedPath string
	sock := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c, err := New(sock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.DeleteRule(PanelRule(9)); err != nil {
		t.Fatal(err)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", capturedMethod)
	}
	if capturedPath != "/v1/rules/panel/9" {
		t.Errorf("path = %q, want /v1/rules/panel/9", capturedPath)
	}
}

func TestClient_HTTPTransport_HealthAndCreate(t *testing.T) {
	var gotAuth string
	srv := http.NewServeMux()
	srv.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv.HandleFunc("/v1/rules", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entry":"10.0.0.1:22","listen_port":22}`))
	})
	httpSrv := &http.Server{Handler: srv}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go httpSrv.Serve(l)
	defer httpSrv.Close()

	addr := "http://" + l.Addr().String()
	c, err := New(addr, WithBearerToken("s3cret"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
	resp, err := c.CreateRule(CreateRuleReq{Proto: "tcp", ExitHost: "10.0.0.1", ExitPort: 22, ListenPort: 22})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", gotAuth)
	}
	if resp.ListenPort != 22 {
		t.Errorf("ListenPort = %d, want 22", resp.ListenPort)
	}
}

func TestClient_UnixTransport_StillWorks(t *testing.T) {
	dir := shortSockDir(t)
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	go func() { _ = http.Serve(ln, mux) }()

	c, err := New("unix://" + sockPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Health(); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestDeleteRule_SurfacesServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rules/panel/9", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rule in use", http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := New(srv.URL)

	err := c.DeleteRule(PanelRule(9))
	if err == nil || !strings.Contains(err.Error(), "rule in use") {
		t.Fatalf("expected server error surfaced, got %v", err)
	}
}

// Verify that nft.Rule is not imported just for the old types; the import
// should still be used by ListRules.
var _ = nft.Rule{}
