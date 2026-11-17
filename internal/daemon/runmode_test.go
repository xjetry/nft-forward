package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// managedModes 是两种托管形态。凡是“托管形态下如何”的断言都对两者各跑一遍：
// 它们只在往哪拨上不同，本机控制口上的行为必须一致。
var managedModes = map[string]RunMode{
	"远端面板托管": RemotePanel("wss://panel.example/v1/agents", "tok"),
	"面板机本机":  LocalPanel("/nonexistent/panel.sock"),
}

// onlineDialer 造一个“会话在线”的拨号器。它没有在跑（没有主循环），只用来让
// panelSession 认为在线；真正把命令送出去的路径由带假面板的测试覆盖。
func onlineDialer(mode RunMode, nodeID int64, nodeName string) *Dialer {
	dl := NewDialer(DialerConfig{Panel: mode})
	dl.connected.Store(true)
	dl.nodeID, dl.nodeName = nodeID, nodeName
	return dl
}

func serve(d *Daemon, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	return w
}

const createBody = `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":80,"listen_port":12000}`

// 托管形态下规则由面板建。面板不可达时本机控制口不替它建：请求被拒绝，本地段里
// 不多出规则，数据面不动。否则这条规则在面板不知情、不计费的情况下开始转发，等
// 重连后再作为一条新规则交给面板接管——而面板可能拒绝接管它。
func TestManagedDaemonRefusesToCreateWhileThePanelIsUnreachable(t *testing.T) {
	for name, mode := range managedModes {
		dp := &fakeDataplane{}
		d := newTestDaemon(t)
		d.dp = dp
		d.mode = mode

		w := serve(d, http.MethodPost, "/v1/rules", createBody)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s：面板不可达时新建应回 503，实际 %d %s", name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "面板不可达") {
			t.Errorf("%s：应说明原因是面板不可达，实际 %q", name, w.Body.String())
		}
		if n := len(d.owners["tui"]); n != 0 {
			t.Errorf("%s：被拒绝的新建不应落进本地段，实际 %d 条", name, n)
		}
		if n := len(dp.nftCalls); n != 0 {
			t.Errorf("%s：被拒绝的新建不应动数据面，实际下发 %d 次", name, n)
		}
	}

	// 对照：单机形态下同一个请求建出一条本地规则。
	d := newTestDaemon(t)
	if w := serve(d, http.MethodPost, "/v1/rules", createBody); w.Code != http.StatusOK {
		t.Fatalf("单机形态下新建应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if n := len(d.owners["tui"]); n != 1 {
		t.Fatalf("单机形态下新建应落进本地段，实际 %d 条", n)
	}
}

// 面板规则的地址在三种情形下各有各的回答：单机形态下这个命名空间是空的（404）；
// 托管但离线时规则存在、只是此刻没法请面板裁决（503）。两者不能混成一个回答，
// 调用方要靠它区分“没有这回事”和“等会儿再试”。
func TestPanelRuleAddressDependsOnRunModeNotOnADialerBeingPresent(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		standalone := newTestDaemon(t)
		if w := serve(standalone, method, "/v1/rules/panel/5", `{"listen_port":21000}`); w.Code != http.StatusNotFound {
			t.Errorf("单机形态 %s 面板规则：应回 404，实际 %d %s", method, w.Code, w.Body.String())
		}
		for name, mode := range managedModes {
			d := newTestDaemon(t)
			d.mode = mode
			if w := serve(d, method, "/v1/rules/panel/5", `{"listen_port":21000}`); w.Code != http.StatusServiceUnavailable {
				t.Errorf("%s离线 %s 面板规则：应回 503，实际 %d %s", name, method, w.Code, w.Body.String())
			}
		}
	}
}

// GET /v1/status 把形态与会话状态分开报：形态是启动时定下的，离线不会让托管形态
// 的 daemon 看起来像单机。
func TestStatusReportsRunModeSeparatelyFromSessionState(t *testing.T) {
	type status struct {
		Mode      string `json:"mode"`
		Connected bool   `json:"connected"`
		NodeName  string `json:"node_name"`
		NodeID    int64  `json:"node_id"`
	}
	get := func(d *Daemon) status {
		t.Helper()
		w := serve(d, http.MethodGet, "/v1/status", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
		var s status
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	if got := get(newTestDaemon(t)); got != (status{Mode: "standalone"}) {
		t.Errorf("单机形态：得到 %+v", got)
	}

	remote := RemotePanel("wss://panel.example/v1/agents", "tok")
	d := newTestDaemon(t)
	d.mode = remote
	if got := get(d); got != (status{Mode: "remote-panel"}) {
		t.Errorf("远端面板托管、会话尚未建立：得到 %+v", got)
	}

	d.dialer.Store(onlineDialer(remote, 9, "edge-9"))
	if got := get(d); got != (status{Mode: "remote-panel", Connected: true, NodeName: "edge-9", NodeID: 9}) {
		t.Errorf("远端面板托管、在线：得到 %+v", got)
	}

	// 会话断开后形态不变，上一次握手得到的节点身份仍然报出来，界面据此说明是哪个
	// 节点离线了。
	d.dialer.Load().connected.Store(false)
	if got := get(d); got != (status{Mode: "remote-panel", NodeName: "edge-9", NodeID: 9}) {
		t.Errorf("远端面板托管、断线：得到 %+v", got)
	}

	local := newTestDaemon(t)
	local.mode = LocalPanel("")
	if got := get(local); got.Mode != "local-panel" {
		t.Errorf("面板机本机：形态应为 local-panel，得到 %+v", got)
	}
}

// 列表里每一行带着自己的地址，由 daemon 按规则所在的归属段给出。两种 id 的取值
// 可以相同（本地 id 是 8 位十六进制，可能全是数字），地址仍然不同。
func TestListedRulesCarryTheAddressOfTheSegmentTheyLiveIn(t *testing.T) {
	d := newTestDaemon(t)
	d.owners = OwnerRuleset{
		"panel": {{RuleID: 12345678, RuleName: "r", Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90}},
		"tui":   {{ID: "12345678", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	w := serve(d, http.MethodGet, "/v1/rules", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	var got struct {
		Rules []struct {
			nft.Rule
			Ref struct {
				NS string `json:"ns"`
				ID string `json:"id"`
			} `json:"ref"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("应列出 2 条，实际 %+v", got.Rules)
	}
	if r := got.Rules[0]; r.SrcPort != 90 || r.Ref.NS != "panel" || r.Ref.ID != "12345678" {
		t.Errorf("面板规则的地址应为 panel/12345678，实际 %+v", r)
	}
	if r := got.Rules[1]; r.SrcPort != 80 || r.Ref.NS != "local" || r.Ref.ID != "12345678" {
		t.Errorf("本地规则的地址应为 local/12345678，实际 %+v", r)
	}
}
