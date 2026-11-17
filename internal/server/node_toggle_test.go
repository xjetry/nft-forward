package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

const (
	chainEntryPort = 12101
	chainNextPort  = 12102
)

// chainEnv 是一个面板、两个已接入的远程节点，和一条依次经过它们的两跳规则。
type chainEnv struct {
	*localAgentEnv
	entry, next   *db.Node
	entryA, nextA *fakeAgent
	uid           int64
	user          *http.Cookie
	ruleID        int64
}

func newChainEnv(t *testing.T) *chainEnv {
	t.Helper()
	e := &chainEnv{localAgentEnv: newLocalAgentEnv(t)}
	mk := func(name string) *db.Node {
		n, err := db.CreateNode(e.d, name, "https://panel.example.com", "tok-"+name)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateNodeRelayHost(e.d, n.ID, "192.0.2."+fmt.Sprint(10+n.ID)); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.entry, e.next = mk("entry"), mk("next")
	e.uid, e.user = loginAsUser(t, e.d, 10)
	if err := db.GrantNode(e.d, e.uid, e.entry.ID, 10, 0); err != nil {
		t.Fatal(err)
	}

	tx, err := e.d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rl := &db.Rule{NodeID: e.entry.ID, OwnerID: sql.NullInt64{Int64: e.uid, Valid: true}, Name: "chain", Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 443}
	if rl.ID, err = db.CreateRule(tx, rl); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	hops := []db.HopInput{{NodeID: e.entry.ID, DesiredPort: chainEntryPort}, {NodeID: e.next.ID, DesiredPort: chainNextPort}}
	if _, _, _, err := db.RegenerateRule(tx, rl, hops, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	e.ruleID = rl.ID

	connect := func(name string) *fakeAgent {
		return connectFakeAgent(t, wsURL(e.public), wsproto.Hello{NodeToken: "tok-" + name, AgentVersion: "v0.0.1", OS: "linux", Arch: "amd64"}, nil)
	}
	e.entryA, e.nextA = connect("entry"), connect("next")
	e.entryA.pushes.waitForwarding(t, chainEntryPort, true, "前置条件：入口节点上线后应收到规则的入口跳")
	e.nextA.pushes.waitForwarding(t, chainNextPort, true, "前置条件：第二个节点上线后应收到规则的第二跳")
	return e
}

// do 以 cookie 的身份调一个面板接口，返回状态码与响应体。
func (e *chainEnv) do(t *testing.T, cookie *http.Cookie, method, path string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	e.s.Router().ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (e *chainEnv) toggle(t *testing.T, nodeID int64) {
	t.Helper()
	if code, body := e.do(t, e.admin, "POST", fmt.Sprintf("/api/nodes/%d/toggle", nodeID)); code != http.StatusOK {
		t.Fatalf("切换节点 %d 的启停应成功，实际 HTTP %d：%s", nodeID, code, body)
	}
}

// 停用链上的一个节点，规则就整条不转发：被停用的节点和链上的其他节点都要立刻撤掉
// 这条规则的跳——接口返回时两边都已收到不含它的规则集。重新启用后两边都恢复。
func TestTogglingANodeWithdrawsAndRestoresRulesThroughIt(t *testing.T) {
	e := newChainEnv(t)

	e.toggle(t, e.next.ID)
	if has, _ := e.nextA.pushes.forwards(chainNextPort); has {
		t.Error("被停用的节点自己应收到不含这条规则的规则集")
	}
	if has, _ := e.entryA.pushes.forwards(chainEntryPort); has {
		t.Error("规则经过被停用的节点，链上的入口节点也应撤掉它的入口跳")
	}

	e.toggle(t, e.next.ID)
	if has, _ := e.nextA.pushes.forwards(chainNextPort); !has {
		t.Error("重新启用后，该节点应重新收到这条规则的跳")
	}
	if has, _ := e.entryA.pushes.forwards(chainEntryPort); !has {
		t.Error("重新启用后，入口节点应重新收到这条规则的入口跳")
	}
}

// 停用的节点仍然可以接入：只有连得上，它才收得到“规则已撤”的那次下发。节点在
// 离线期间被停用、之后带着旧规则集重连时，握手被接受，随后收到的规则集里没有任何
// 规则；链上其他节点上的跳也已撤掉。
func TestNodeDisabledWhileOfflineConvergesToNoRulesOnReconnect(t *testing.T) {
	e := newChainEnv(t)
	e.nextA.conn.CloseNow()
	waitFor(t, "第二个节点下线", func() bool { return !e.s.Hub.IsOnline(e.next.ID) })

	e.toggle(t, e.next.ID)
	if has, _ := e.entryA.pushes.forwards(chainEntryPort); has {
		t.Error("节点离线时被停用，链上在线的入口节点也应立刻撤掉这条规则的入口跳")
	}

	back := connectFakeAgent(t, wsURL(e.public), wsproto.Hello{NodeToken: "tok-next", AgentVersion: "v0.0.1", OS: "linux", Arch: "amd64", LastAppliedRev: "rev-from-before"}, nil)
	waitFor(t, "停用的节点重连后收到一次下发", func() bool { return back.pushes.count() > 0 })
	if has, _ := back.pushes.forwards(chainNextPort); has {
		t.Fatal("停用的节点重连后收到的规则集里不应有任何经过它的规则")
	}
}

// self 节点被停用时同样可以从本机入口接入，并且收不到规则。
func TestDisabledSelfNodeConnectsAndReceivesNoRules(t *testing.T) {
	e := newLocalAgentEnv(t)
	createStandaloneRuleHop(t, e.d, e.self.ID, "tcp", selfRulePort, "10.0.0.30", 9200, sql.NullInt64{})
	if _, err := e.d.Exec(`UPDATE nodes SET disabled=1 WHERE id=?`, e.self.ID); err != nil {
		t.Fatal(err)
	}

	a := connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64", LastAppliedRev: "rev-from-before"}, nil)
	waitFor(t, "停用的 self 节点接入后收到一次下发", func() bool { return a.pushes.count() > 0 })
	if has, _ := a.pushes.forwards(selfRulePort); has {
		t.Fatal("停用的 self 节点收到的规则集里不应有规则")
	}
}

// 一条规则没生效时，它的每一个对外面都要报出同一个原因：管理员的列表与详情、
// 属主的列表与详情、/api/v1 的列表与详情。规则照常转发时每一面都不带这个字段。
func TestEveryRuleSurfaceReportsTheSuppression(t *testing.T) {
	e := newChainEnv(t)
	tok, err := db.CreateAPIToken(e.d, e.uid, "read")
	if err != nil {
		t.Fatal(err)
	}

	// 每一面取出这条规则的 suppressed 字段；present 报告字段是否出现在 JSON 里。
	type surface struct {
		name string
		get  func() (value string, present bool)
	}
	field := func(raw json.RawMessage) (string, bool) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("规则对象解码: %v（%s）", err, raw)
		}
		v, ok := m["suppressed"]
		if !ok {
			return "", false
		}
		var s string
		_ = json.Unmarshal(v, &s)
		return s, true
	}
	pick := func(list []json.RawMessage) json.RawMessage {
		for _, raw := range list {
			var head struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(raw, &head)
			if head.ID == e.ruleID {
				return raw
			}
		}
		t.Fatalf("列表里找不到规则 %d", e.ruleID)
		return nil
	}
	cookieList := func(c *http.Cookie, path string) func() (string, bool) {
		return func() (string, bool) {
			code, body := e.do(t, c, "GET", path)
			if code != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d %s", path, code, body)
			}
			var out struct {
				Rules []json.RawMessage `json:"rules"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("GET %s 解码: %v（%s）", path, err, body)
			}
			return field(pick(out.Rules))
		}
	}
	cookieDetail := func(c *http.Cookie, path string) func() (string, bool) {
		return func() (string, bool) {
			code, body := e.do(t, c, "GET", path)
			if code != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d %s", path, code, body)
			}
			var out struct {
				Rule json.RawMessage `json:"rule"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("GET %s 解码: %v（%s）", path, err, body)
			}
			return field(out.Rule)
		}
	}
	v1 := func(path string, list bool) func() (string, bool) {
		return func() (string, bool) {
			rec := v1Do(t, e.s, "GET", path, tok, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: HTTP %d %s", path, rec.Code, rec.Body.String())
			}
			var env struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("GET %s 解码: %v（%s）", path, err, rec.Body.String())
			}
			if !list {
				return field(env.Data)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(env.Data, &items); err != nil {
				t.Fatalf("GET %s 的 data 应是数组: %v（%s）", path, err, env.Data)
			}
			return field(pick(items))
		}
	}
	detail := fmt.Sprintf("/rules/%d", e.ruleID)
	surfaces := []surface{
		{"管理员列表", cookieList(e.admin, "/api/rules")},
		{"管理员详情", cookieDetail(e.admin, "/api"+detail)},
		{"属主列表", cookieList(e.user, "/api/my/rules")},
		{"属主详情", cookieDetail(e.user, "/api/my"+detail)},
		{"v1 列表", v1("/api/v1/my/rules", true)},
		{"v1 详情", v1("/api/v1/my"+detail, false)},
	}

	for _, sf := range surfaces {
		if v, present := sf.get(); present {
			t.Errorf("%s：规则照常转发时不应带 suppressed 字段，实际 %q", sf.name, v)
		}
	}

	e.toggle(t, e.next.ID)

	for _, sf := range surfaces {
		if v, _ := sf.get(); v != string(db.SuppressNodeDisabled) {
			t.Errorf("%s：规则因节点停用没生效，suppressed 应为 %q，实际 %q", sf.name, db.SuppressNodeDisabled, v)
		}
	}
}
