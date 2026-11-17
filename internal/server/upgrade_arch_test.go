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

// testPanelVersion 是这些测试里面板自居的 release 版本；connectAgent 默认上报
// 的 v0.0.1 低于它，所以是批量升级的对象。
const testPanelVersion = "v9.9.9"

// panelAt 让面板在本测试期间以给定版本自居。测试进程是 dev 构建，而对 dev
// 面板来说没有哪个节点算“版本落后”（agentOutdated）。要在预置产物之前调用：
// seedAgentArtifacts 用的是调用那一刻的面板版本。
func panelAt(t *testing.T, version string) {
	t.Helper()
	prev := serverVersion
	serverVersion = func() string { return version }
	t.Cleanup(func() { serverVersion = prev })
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
	return e.connectAgentAt(t, name, arch, sha, "v0.0.1")
}

// connectAgentAt 同 connectAgent，另外指定 hello 里上报的 agent 版本。
func (e *upgradeTestEnv) connectAgentAt(t *testing.T, name, arch, sha, version string) (*db.Node, <-chan wsproto.Upgrade) {
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
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-" + name, AgentVersion: version, AgentSHA: sha, OS: "linux", Arch: arch})
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
	panelAt(t, testPanelVersion)
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64", "arm64")
	x86, x86Ch := e.connectAgent(t, "x86", "amd64", "stale")
	arm, armCh := e.connectAgent(t, "arm", "arm64", "stale")
	rv, rvCh := e.connectAgent(t, "rv", "riscv64", "stale")
	// 上报过旧版本、此刻不在线的节点：是升级对象，但没有连接可推。
	offline, err := db.CreateNode(e.d, "offline", "https://panel.example.com", "tok-offline")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Exec(`UPDATE nodes SET agent_version='v0.0.1' WHERE id=?`, offline.ID); err != nil {
		t.Fatal(err)
	}

	code, body := e.post(t, "/api/nodes/upgrade-all")
	if code != http.StatusOK {
		t.Fatalf("upgrade-all http=%d body=%s", code, body)
	}
	var resp struct{ Upgraded, Offline, Failed int }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Upgraded != 2 || resp.Offline != 1 || resp.Failed != 1 {
		t.Errorf("upgraded/offline/failed = %d/%d/%d, want 2 (amd64 + arm64) / 1 (offline) / 1 (riscv64); body=%s",
			resp.Upgraded, resp.Offline, resp.Failed, body)
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
	panelAt(t, testPanelVersion)
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

// 批量升级的对象是“已启用且版本低于面板”的节点，不是所有节点：已经不低于面板
// 版本的不推（对它们要么是空操作，要么是降级），被禁用的不推，从未上报过版本
// 的（还没装 agent）既不推也不记一笔失败。
//
// 同时钉住跨面的一致：节点列表给出的 outdated_node_ids 与这里的推送对象出自
// 同一个判定，推送对象恰好是其中已启用的那些。
func TestUpgradeAllTargetsEnabledOutdatedNodes(t *testing.T) {
	panelAt(t, testPanelVersion)
	e := newUpgradeTestEnv(t)
	seedAgentArtifacts(t, "amd64")
	old, oldCh := e.connectAgentAt(t, "old", "amd64", "stale", "v9.9.8")
	current, currentCh := e.connectAgentAt(t, "current", "amd64", "other-build", testPanelVersion)
	newer, newerCh := e.connectAgentAt(t, "newer", "amd64", "other-build", "v10.0.0")
	disabled, disabledCh := e.connectAgentAt(t, "disabled", "amd64", "stale", "v9.9.8")
	if err := db.ToggleNode(e.d, disabled.ID); err != nil {
		t.Fatal(err)
	}
	notInstalled, err := db.CreateNode(e.d, "not-installed", "https://panel.example.com", "tok-not-installed")
	if err != nil {
		t.Fatal(err)
	}

	// 节点列表上的“可升级”集合。
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/nodes", nil)
	req.AddCookie(e.cookie)
	listResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Outdated []int64 `json:"outdated_node_ids"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	listResp.Body.Close()
	marked := map[int64]bool{}
	for _, id := range list.Outdated {
		marked[id] = true
	}
	for _, tc := range []struct {
		node *db.Node
		want bool
	}{{old, true}, {disabled, true}, {current, false}, {newer, false}, {notInstalled, false}} {
		if marked[tc.node.ID] != tc.want {
			t.Errorf("node list marks %s outdated=%v, want %v", tc.node.Name, marked[tc.node.ID], tc.want)
		}
	}

	code, body := e.post(t, "/api/nodes/upgrade-all")
	if code != http.StatusOK {
		t.Fatalf("upgrade-all http=%d body=%s", code, body)
	}
	var resp struct{ Upgraded, Offline, Failed int }
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Upgraded != 1 || resp.Offline != 0 || resp.Failed != 0 {
		t.Errorf("upgraded/offline/failed = %d/%d/%d, want 1/0/0; body=%s", resp.Upgraded, resp.Offline, resp.Failed, body)
	}
	assertFullPush(t, received(t, oldCh, "outdated node"), "amd64", "outdated node")
	assertNoUpgrade(t, currentCh, "node at the panel version")
	assertNoUpgrade(t, newerCh, "node newer than the panel")
	assertNoUpgrade(t, disabledCh, "disabled node")

	// 只有真正推送过的节点留下升级记录。
	for _, tc := range []struct {
		node   *db.Node
		status string
	}{{old, "acked"}, {current, ""}, {newer, ""}, {disabled, ""}, {notInstalled, ""}} {
		got, err := db.GetNode(e.d, tc.node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastUpgradeStatus != tc.status {
			t.Errorf("%s: recorded upgrade status=%q (err=%q), want %q", tc.node.Name, got.LastUpgradeStatus, got.LastUpgradeError, tc.status)
		}
	}

	// 节点详情给出同一个判定的结果，以及这次推送的状态：被推送的节点（测试里
	// 的 agent 不会真的换版本）在宽限期内是 pending，没被推送的没有记录。
	for _, tc := range []struct {
		node     *db.Node
		outdated bool
		status   string
	}{{old, true, "pending"}, {disabled, true, "none"}, {current, false, "none"}, {newer, false, "none"}} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/nodes/"+strconv.FormatInt(tc.node.ID, 10), nil)
		req.AddCookie(e.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var detail struct {
			AgentOutdated *bool `json:"agent_outdated"`
			Upgrade       struct {
				Status string `json:"status"`
			} `json:"upgrade"`
		}
		err = json.NewDecoder(resp.Body).Decode(&detail)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if detail.AgentOutdated == nil || *detail.AgentOutdated != tc.outdated {
			t.Errorf("node detail of %s: agent_outdated=%v, want %v", tc.node.Name, detail.AgentOutdated, tc.outdated)
		}
		if detail.Upgrade.Status != tc.status {
			t.Errorf("node detail of %s: upgrade status=%q, want %q", tc.node.Name, detail.Upgrade.Status, tc.status)
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
