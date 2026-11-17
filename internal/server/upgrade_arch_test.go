package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// seedAgentArtifacts 预置各架构的产物缓存：测试进程是 dev 构建，没有可下载的
// release。每个架构的字节与 sha 互不相同，断言才能分辨“推的是哪一份”。
func seedAgentArtifacts(t *testing.T, archs ...string) {
	t.Helper()
	agentArtMu.Lock()
	for _, arch := range archs {
		agentArtCache[arch] = &agentArtifact{
			Version: serverVersion(), Arch: arch,
			SHA: "sha-" + arch, Data: []byte("agent-bytes-" + arch),
		}
	}
	agentArtMu.Unlock()
	t.Cleanup(func() {
		agentArtMu.Lock()
		agentArtCache = map[string]*agentArtifact{}
		agentArtMu.Unlock()
	})
}

type upgradeTestEnv struct {
	d      *sql.DB
	srv    *httptest.Server
	cookie *http.Cookie
}

func newUpgradeTestEnv(t *testing.T) *upgradeTestEnv {
	t.Helper()
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	hash, _ := HashPassword("pw")
	aid, err := db.CreateUser(d, "admin1", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateSession(d, aid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &upgradeTestEnv{d: d, srv: srv, cookie: &http.Cookie{Name: sessionCookie, Value: tok}}
}

func (e *upgradeTestEnv) post(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(e.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// connectAgent 以给定架构 / sha 的身份走真实的 /v1/agents 握手，之后在后台
// 应答面板发来的帧，并把收到的 upgrade 帧原样交给返回的 channel。
func (e *upgradeTestEnv) connectAgent(t *testing.T, name, arch, sha string) (*db.Node, <-chan wsproto.Upgrade) {
	t.Helper()
	n, err := db.CreateNode(e.d, name, "https://panel.example.com", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-" + name, AgentVersion: "v0.0.1", AgentSHA: sha, OS: "linux", Arch: arch})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	var ack wsproto.HelloAck
	if env := recvEnvelope(t, c); json.Unmarshal(env.Payload, &ack) != nil || ack.Error != "" || ack.NodeID != n.ID {
		t.Fatalf("hello for %s rejected: %+v", name, ack)
	}

	upgrades := make(chan wsproto.Upgrade, 4)
	reply := func(typ, id string, v any) {
		p, _ := json.Marshal(v)
		b, _ := json.Marshal(wsproto.Envelope{Type: typ, ID: id, Payload: p})
		_ = c.Write(context.Background(), websocket.MessageText, b)
	}
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if json.Unmarshal(b, &env) != nil {
				return
			}
			switch env.Type {
			case wsproto.TypeApplyRuleset:
				var a wsproto.ApplyRuleset
				_ = json.Unmarshal(env.Payload, &a)
				reply(wsproto.TypeApplyAck, env.ID, wsproto.ApplyAck{Rev: a.Rev, OK: true})
			case wsproto.TypeUpgrade:
				var u wsproto.Upgrade
				_ = json.Unmarshal(env.Payload, &u)
				upgrades <- u
				reply(wsproto.TypeUpgradeAck, env.ID, wsproto.UpgradeAck{OK: true})
			}
		}
	}()
	return n, upgrades
}

// received 取一条已到达的 upgrade 帧。只在面板的 HTTP 应答返回之后调用：SendUpgrade
// 要等到 upgrade_ack 才返回，而 agent 是先入队再应答，所以此时帧必然已在 channel 里。
func received(t *testing.T, ch <-chan wsproto.Upgrade, who string) wsproto.Upgrade {
	t.Helper()
	select {
	case u := <-ch:
		return u
	default:
		t.Fatalf("%s: expected an upgrade frame, got none", who)
		return wsproto.Upgrade{}
	}
}

func assertNoUpgrade(t *testing.T, ch <-chan wsproto.Upgrade, who string) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("%s: must not receive an upgrade frame, got sha=%s data=%q", who, u.SHA256, u.Data)
	default:
	}
}

// assertFullPush 断言 u 是 arch 那一份产物的完整推送：字节、sha 与回退下载地址
// 三者都指向同一架构。
func assertFullPush(t *testing.T, u wsproto.Upgrade, arch, who string) {
	t.Helper()
	if string(u.Data) != "agent-bytes-"+arch {
		t.Errorf("%s: pushed bytes = %q, want the %s artifact", who, u.Data, arch)
	}
	if u.SHA256 != "sha-"+arch {
		t.Errorf("%s: pushed sha = %q, want sha-%s", who, u.SHA256, arch)
	}
	if !strings.HasSuffix(u.DownloadAt, "/v1/binary?arch="+arch) {
		t.Errorf("%s: fallback url = %q, want it to name arch=%s", who, u.DownloadAt, arch)
	}
}

func TestAgentAssetName(t *testing.T) {
	for arch, want := range map[string]string{"amd64": "nft-agent", "arm64": "nft-agent-arm64"} {
		got, err := agentAssetName(arch)
		if err != nil || got != want {
			t.Errorf("agentAssetName(%q) = %q, %v; want %q", arch, got, err, want)
		}
	}
	// 未知架构与空串都必须报错，不能回落到某个默认产物。
	for _, arch := range []string{"", "arm", "riscv64", "AMD64", "x86_64", "aarch64"} {
		if got, err := agentAssetName(arch); err == nil {
			t.Errorf("agentAssetName(%q) = %q, want an error", arch, got)
		}
	}
}

// 单节点升级：每个节点收到的必须是它自己架构的产物——两个方向都钉。
func TestUpgradeNodePushesArtifactOfNodeArch(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")
	x86, x86Ch := e.connectAgent(t, "x86", "amd64", "stale")
	arm, armCh := e.connectAgent(t, "arm", "arm64", "stale")

	for _, tc := range []struct {
		node *db.Node
		ch   <-chan wsproto.Upgrade
		arch string
	}{{x86, x86Ch, "amd64"}, {arm, armCh, "arm64"}} {
		code, body := e.post(t, "/api/nodes/"+strconv.FormatInt(tc.node.ID, 10)+"/upgrade")
		if code != http.StatusOK {
			t.Fatalf("%s: upgrade http=%d body=%s", tc.arch, code, body)
		}
		assertFullPush(t, received(t, tc.ch, tc.arch), tc.arch, tc.arch+" node")
	}
	// 每个节点只该收到自己那一次推送。
	assertNoUpgrade(t, x86Ch, "amd64 node")
	assertNoUpgrade(t, armCh, "arm64 node")
}

// sha 是否“已是目标版本”也按节点自己架构的产物比：arm64 节点跑着 arm64 产物时
// 只同步版本标签，不重推字节。
func TestUpgradeNodeLabelOnlySyncComparesAgainstOwnArch(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")
	arm, armCh := e.connectAgent(t, "arm", "arm64", "sha-arm64")

	code, body := e.post(t, "/api/nodes/"+strconv.FormatInt(arm.ID, 10)+"/upgrade")
	if code != http.StatusOK {
		t.Fatalf("upgrade http=%d body=%s", code, body)
	}
	u := received(t, armCh, "arm64 node")
	if len(u.Data) != 0 || u.DownloadAt != "" {
		t.Fatalf("node already on the arm64 artifact must get a label-only sync, got data=%q url=%q", u.Data, u.DownloadAt)
	}
	if u.SHA256 != "sha-arm64" || u.Version != serverVersion() {
		t.Fatalf("label-only sync must carry the arm64 sha + panel version, got %+v", u)
	}
}

// 没有对应产物的架构：拒绝推送并把原因记到节点上，一个字节都不发。
func TestUpgradeNodeRejectsArchWithoutArtifact(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")
	rv, rvCh := e.connectAgent(t, "rv", "riscv64", "stale")

	code, body := e.post(t, "/api/nodes/"+strconv.FormatInt(rv.ID, 10)+"/upgrade")
	if code != http.StatusInternalServerError || !strings.Contains(body, "riscv64") {
		t.Fatalf("want 500 naming the arch, got http=%d body=%s", code, body)
	}
	assertNoUpgrade(t, rvCh, "riscv64 node")
	got, _ := db.GetNode(e.d, rv.ID)
	if got.LastUpgradeStatus != "error" || !strings.Contains(got.LastUpgradeError, "riscv64") {
		t.Fatalf("rejection must be recorded on the node, got status=%q err=%q", got.LastUpgradeStatus, got.LastUpgradeError)
	}
}

// 批量升级：各架构各取各的产物；没有产物的架构与离线节点是节点级失败，不拦别人。
func TestUpgradeAllPushesPerArch(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")
	x86, x86Ch := e.connectAgent(t, "x86", "amd64", "stale")
	arm, armCh := e.connectAgent(t, "arm", "arm64", "stale")
	rv, rvCh := e.connectAgent(t, "rv", "riscv64", "stale")
	offline, err := db.CreateNode(e.d, "offline", "https://panel.example.com", "tok-offline")
	if err != nil {
		t.Fatal(err)
	}

	code, body := e.post(t, "/api/nodes/upgrade-all")
	if code != http.StatusOK {
		t.Fatalf("upgrade-all http=%d body=%s", code, body)
	}
	var resp struct{ Upgraded, Failed int }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Upgraded != 2 {
		t.Errorf("upgraded = %d, want 2 (amd64 + arm64); body=%s", resp.Upgraded, body)
	}

	assertFullPush(t, received(t, x86Ch, "amd64 node"), "amd64", "amd64 node")
	assertFullPush(t, received(t, armCh, "arm64 node"), "arm64", "arm64 node")
	assertNoUpgrade(t, rvCh, "riscv64 node")

	for _, tc := range []struct {
		node            *db.Node
		status, errPart string
	}{
		{x86, "acked", ""},
		{arm, "acked", ""},
		{rv, "error", "riscv64"},
		{offline, "error", errNodeNotConnected.Error()},
	} {
		got, _ := db.GetNode(e.d, tc.node.ID)
		if got.LastUpgradeStatus != tc.status || !strings.Contains(got.LastUpgradeError, tc.errPart) {
			t.Errorf("%s: recorded status=%q err=%q, want status=%q err containing %q",
				tc.node.Name, got.LastUpgradeStatus, got.LastUpgradeError, tc.status, tc.errPart)
		}
	}
}

// 批量升级：在线节点用得到的产物只要有一个取不到，整批失败且一个都不推、不记账。
// 这里只预置了 amd64，arm64 节点所需的产物在 dev 构建下取不到。
func TestUpgradeAllFailsWholeBatchWhenAnArtifactIsUnavailable(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64")
	x86, x86Ch := e.connectAgent(t, "x86", "amd64", "stale")
	arm, armCh := e.connectAgent(t, "arm", "arm64", "stale")

	code, body := e.post(t, "/api/nodes/upgrade-all")
	if code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the arm64 artifact is unavailable, got http=%d body=%s", code, body)
	}
	assertNoUpgrade(t, x86Ch, "amd64 node")
	assertNoUpgrade(t, armCh, "arm64 node")
	for _, n := range []*db.Node{x86, arm} {
		got, _ := db.GetNode(e.d, n.ID)
		if got.LastUpgradeStatus != "" {
			t.Errorf("%s: nothing was attempted, but an upgrade result was recorded: status=%q err=%q",
				n.Name, got.LastUpgradeStatus, got.LastUpgradeError)
		}
	}
}

// 回退下载口按 arch 参数返回对应产物；缺省或未知架构一律 400，不回落到默认产物。
func TestServeBinarySelectsByArch(t *testing.T) {
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")

	get := func(query string) (int, string, string) {
		resp, err := http.Get(e.srv.URL + "/v1/binary" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("X-SHA256")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		code, body, sha := get("?arch=" + arch)
		if code != http.StatusOK || body != "agent-bytes-"+arch || sha != "sha-"+arch {
			t.Errorf("arch=%s: http=%d body=%q sha=%q, want that arch's artifact", arch, code, body, sha)
		}
	}
	for _, query := range []string{"", "?arch=", "?arch=riscv64"} {
		if code, body, _ := get(query); code != http.StatusBadRequest {
			t.Errorf("GET /v1/binary%s: http=%d body=%q, want 400", query, code, body)
		}
	}
}
