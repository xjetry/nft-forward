package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// localAgentEnv 是一个面板加它的两个 agent 入口：public 是对外的 HTTP 路由（节点凭
// token 接入），local 是只给本机 daemon 用的入口（生产中挂在 unix socket 上，这里用
// 回环 TCP 承载——入口的行为与承载它的传输无关）。
type localAgentEnv struct {
	d      *sql.DB
	s      *Server
	public *httptest.Server
	local  *httptest.Server
	self   *db.Node
	admin  *http.Cookie
}

func newLocalAgentEnv(t *testing.T) *localAgentEnv {
	t.Helper()
	d := openDB(t)
	// 与面板进程的启动顺序一致（cmd/nft-server 先 bootstrap 管理员再 server.New）：
	// self 节点的属主在 New 里取“第一个管理员”，管理员得先存在。
	admin := loginAsAdmin(t, d)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	e := &localAgentEnv{d: d, s: s, admin: admin}
	e.public = httptest.NewServer(s.Router())
	t.Cleanup(e.public.Close)
	e.local = httptest.NewServer(s.LocalAgentHandler())
	t.Cleanup(e.local.Close)
	if e.self, err = db.GetSelfNode(d); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *localAgentEnv) selfNode(t *testing.T) *db.Node {
	t.Helper()
	n, err := db.GetNode(e.d, e.self.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// waitFor 轮询 cond 直到成立或超时。面板处理一帧是异步的，测试不能假设它已经完成。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const selfRulePort = 12002

// 本机 daemon 从本机入口握手，不带 token，被认定为 self 节点；之后它与远程节点走
// 完全相同的通道：在线状态来自这条连接，规则经这条连接下发。
func TestLocalAgentConnectsAsSelfNode(t *testing.T) {
	e := newLocalAgentEnv(t)
	if n := e.selfNode(t); n.Online != 0 {
		t.Fatalf("前置条件：daemon 还没连上时 self 节点应为离线，online=%d", n.Online)
	}
	createStandaloneRuleHop(t, e.d, e.self.ID, "tcp", selfRulePort, "10.0.0.30", 9200, sql.NullInt64{})

	a := connectFakeAgent(t, wsURL(e.local), wsproto.Hello{AgentVersion: "v9.9.9", AgentSHA: "sha-local", OS: "linux", Arch: "amd64"}, nil)

	if a.ack.NodeID != e.self.ID {
		t.Fatalf("本机入口的连接应被认定为 self 节点 %d，实际 node_id=%d", e.self.ID, a.ack.NodeID)
	}
	if !e.s.Hub.IsOnline(e.self.ID) {
		t.Fatal("握手成功后 self 节点应在连接表里")
	}
	n := e.selfNode(t)
	if n.Online != 1 || n.AgentVersion != "v9.9.9" || n.AgentSHA != "sha-local" {
		t.Fatalf("self 节点应记录为在线并带上 agent 的版本信息：online=%d version=%q sha=%q", n.Online, n.AgentVersion, n.AgentSHA)
	}
	// 上线后的对账下发走的就是这条连接。
	a.pushes.waitForwarding(t, selfRulePort, true, "self 节点上线后应经 WS 收到它的规则")

	a.conn.CloseNow()
	waitFor(t, "连接断开后 self 节点应转为离线", func() bool { return e.selfNode(t).Online == 0 })
}

// self 节点不能从对外入口冒充：它没有 token（secret 为空），而对外入口只按 token
// 认节点，空 token 一律拒绝。
func TestSelfNodeCannotBeClaimedOverTheNetwork(t *testing.T) {
	e := newLocalAgentEnv(t)
	if e.self.Secret != "" {
		t.Fatalf("self 节点不应有 token，否则持有它的人可以从网络上冒充本机节点：secret=%q", e.self.Secret)
	}
	for _, token := range []string{"", "self"} {
		_, ack := helloOver(t, wsURL(e.public), wsproto.Hello{NodeToken: token, OS: "linux", Arch: "amd64"})
		if ack.Error == "" {
			t.Fatalf("token=%q 在对外入口握手应被拒绝，实际被接受为节点 %d", token, ack.NodeID)
		}
	}
	if e.s.Hub.IsOnline(e.self.ID) {
		t.Fatal("被拒绝的握手不应让 self 节点上线")
	}
}

// 反方向：本机入口只认 self 节点，不看 hello 里的 token——带着别的节点的 token 从
// 本机入口进来，得到的仍是 self 节点，不会变成那个节点。
func TestLocalEntranceIgnoresTheToken(t *testing.T) {
	e := newLocalAgentEnv(t)
	other, err := db.CreateNode(e.d, "edge", "", "tok-edge")
	if err != nil {
		t.Fatal(err)
	}
	_, ack := helloOver(t, wsURL(e.local), wsproto.Hello{NodeToken: "tok-edge", OS: "linux", Arch: "amd64"})
	if ack.Error != "" || ack.NodeID != e.self.ID {
		t.Fatalf("本机入口应认定为 self 节点 %d，实际 node_id=%d error=%q", e.self.ID, ack.NodeID, ack.Error)
	}
	if e.s.Hub.IsOnline(other.ID) {
		t.Fatal("本机入口不应让别的节点上线")
	}
}

// 本机节点转发的流量与远程节点一样入账：用户的已用流量、规则的累计流量、节点的
// 原始流量都要增长。
func TestSelfNodeTrafficIsBilled(t *testing.T) {
	e := newLocalAgentEnv(t)
	uid, _ := loginAsUser(t, e.d, 10)
	if err := db.GrantNode(e.d, uid, e.self.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	_, hopID := createStandaloneRuleHop(t, e.d, e.self.ID, "tcp", selfRulePort, "10.0.0.30", 9200, sql.NullInt64{Int64: uid, Valid: true})
	a := connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64"}, nil)
	a.pushes.waitForwarding(t, selfRulePort, true, "前置条件：规则已下发到 self 节点")

	a.reportTraffic(t, selfRulePort, 700, 500)

	waitFor(t, "self 节点上报的计数应记到用户的已用流量上", func() bool {
		u, _ := db.GetUserByID(e.d, uid)
		return u.TrafficUsedBytes == 1200
	})
	var ruleTotal int64
	if err := e.d.QueryRow(`SELECT total_bytes FROM rule_hops WHERE id=?`, hopID).Scan(&ruleTotal); err != nil {
		t.Fatal(err)
	}
	if ruleTotal != 1200 {
		t.Errorf("规则累计流量应为 1200，实际 %d", ruleTotal)
	}
	raw, err := db.NodeRawTraffic(e.d)
	if err != nil {
		t.Fatal(err)
	}
	if raw[e.self.ID] != 1200 {
		t.Errorf("self 节点的原始流量应为 1200，实际 %d", raw[e.self.ID])
	}
	g, err := db.GetNodeGrant(e.d, uid, e.self.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.TrafficUsedBytes != 1200 {
		t.Errorf("用户在 self 节点上的授权已用量应为 1200，实际 %d", g.TrafficUsedBytes)
	}
}

// 对 self 节点的探测经它的连接送达 agent，而不是恒报未连接。
func TestProbeReachesSelfNode(t *testing.T) {
	e := newLocalAgentEnv(t)
	probe := func() probeResult {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/probe?target=9.9.9.9:80&node=%d", e.public.URL, e.self.ID), nil)
		req.AddCookie(e.admin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res probeResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := probe(); res.OK || res.Error == "" {
		t.Fatalf("前置条件：daemon 未连接时探测应失败并说明原因，实际 %+v", res)
	}

	connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64"}, func(a *fakeAgent) {
		a.probe = wsproto.ProbeAck{OK: true, Latency: 7}
	})

	if res := probe(); !res.OK || res.Latency != 7 {
		t.Fatalf("daemon 连上后探测应由它应答（ok, 7ms），实际 %+v", res)
	}
}

// 本机连接没有网络地址，不能拿“对端地址”去回填数据面地址（回环或 socket 名都不是
// 别的节点能连到的地址）；回填用 agent 自己探测到的出口地址。页面上的连接来源如实
// 显示为本机。
func TestLocalAgentRelayHostComesFromProbedAddress(t *testing.T) {
	e := newLocalAgentEnv(t)
	if e.self.RelayHost != "" {
		t.Fatalf("前置条件：self 节点的数据面地址应为空，实际 %q", e.self.RelayHost)
	}
	connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64", ProbedV4: "203.0.113.7"}, nil)

	n := e.selfNode(t)
	if n.RelayHost != "203.0.113.7" {
		t.Errorf("数据面地址应取 agent 自探测的 203.0.113.7，实际 %q", n.RelayHost)
	}
	if n.Address != localAgentOrigin {
		t.Errorf("连接来源应显示为 %q，实际 %q", localAgentOrigin, n.Address)
	}
}

// self 节点的属主是管理员，而管理员没有规则数上限（管理员账号的 max_rules 默认是 0，
// 管理员的 HTTP 建规则接口从不检查它）。所以面板机上原有的本地规则在 daemon 接入时
// 能被面板接管，之后从节点侧（TUI）也能照常建规则。
func TestSelfNodeAdoptsLocalRulesAndCreatesFromTheNodeSide(t *testing.T) {
	e := newLocalAgentEnv(t)
	self := e.selfNode(t)
	if self.OwnerID == nil {
		t.Fatal("前置条件：self 节点的属主应为管理员")
	}
	owner, err := db.GetUserByID(e.d, *self.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Role != "admin" || owner.MaxRules != 0 {
		t.Fatalf("前置条件：属主应是 max_rules=0 的管理员，实际 role=%s max_rules=%d", owner.Role, owner.MaxRules)
	}
	a := connectFakeAgent(t, wsURL(e.local), wsproto.Hello{OS: "linux", Arch: "amd64", ProbedV4: "203.0.113.7"}, nil)

	ack := a.ruleCmd(t, wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: []nft.Rule{
		{ID: "f1a92408", Proto: "tcp", SrcPort: selfRulePort, DestIP: "198.51.100.9", DestPort: 80, Comment: "local-one"},
	}})
	if !ack.OK {
		t.Fatalf("self 节点的本地规则应被面板接管，实际被拒: %s", ack.Error)
	}
	a.pushes.waitForwarding(t, selfRulePort, true, "接管后的规则应作为面板规则下发回节点")

	ack = a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{
		Proto: "tcp", ExitHost: "198.51.100.10", ExitPort: 80, ListenPort: selfRulePort + 1, Name: "from-tui",
	})
	if !ack.OK {
		t.Fatalf("self 节点从节点侧建规则应成功，实际被拒: %s", ack.Error)
	}
	if n, err := db.CountRulesForUser(e.d, owner.ID); err != nil || n != 2 {
		t.Fatalf("两条规则都应归管理员所有，实际 %d 条 (err=%v)", n, err)
	}
}

// 反方向：普通用户名下的节点仍受该用户的规则数上限约束，超出时整批迁移被拒、一条
// 都不落库；从节点侧建规则同样被拒。
func TestNodeSideRuleCommandsStillCapRegularOwners(t *testing.T) {
	e := newLocalAgentEnv(t)
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(e.d, "capped", hash, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Exec(`UPDATE users SET max_rules=1 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	node, err := db.CreateNode(e.d, "user-edge", "", "tok-user-edge")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeOwner(e.d, node.ID, &uid); err != nil {
		t.Fatal(err)
	}
	// 属主要用自己的节点同样需要授权；授权的条数上限给足，让下面咬住的是账号的上限。
	if err := db.GrantNode(e.d, uid, node.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	a := connectFakeAgent(t, wsURL(e.public), wsproto.Hello{NodeToken: "tok-user-edge", OS: "linux", Arch: "amd64", ProbedV4: "203.0.113.8"}, nil)

	ack := a.ruleCmd(t, wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: []nft.Rule{
		{ID: "a1", Proto: "tcp", SrcPort: 12011, DestIP: "198.51.100.9", DestPort: 80},
		{ID: "a2", Proto: "tcp", SrcPort: 12012, DestIP: "198.51.100.9", DestPort: 81},
	}})
	if ack.OK {
		t.Fatal("超出上限的迁移应被拒绝")
	}
	if n, _ := db.CountRulesForUser(e.d, uid); n != 0 {
		t.Fatalf("被拒的迁移不应落库任何规则，实际 %d 条", n)
	}

	// 上限为 1：第一条能建，第二条被拒。
	if ack = a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 80, ListenPort: 12011, Name: "one"}); !ack.OK {
		t.Fatalf("上限内的规则应能建，实际被拒: %s", ack.Error)
	}
	if ack = a.ruleCmd(t, wsproto.TypeRuleCreate, wsproto.RuleCreate{Proto: "tcp", ExitHost: "198.51.100.9", ExitPort: 81, ListenPort: 12012, Name: "two"}); ack.OK {
		t.Fatal("超出上限的规则应被拒绝")
	}
	if n, _ := db.CountRulesForUser(e.d, uid); n != 1 {
		t.Fatalf("应只有上限内的那一条规则，实际 %d 条", n)
	}
}

// 面板启动时连接表是空的，所以 nodes.online 必须全为 0：上一个进程退出时没来得及
// 标记离线的节点（含 self 节点）不能在新进程里继续显示在线。
func TestPanelStartMarksEveryNodeOffline(t *testing.T) {
	d := openDB(t)
	if _, err := New(d); err != nil {
		t.Fatal(err)
	}
	remote, err := db.CreateNode(d, "edge", "", "tok-edge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET online=1`); err != nil {
		t.Fatal(err)
	}

	if _, err := New(d); err != nil { // 面板重启
		t.Fatal(err)
	}

	self, err := db.GetSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if self.Online != 0 {
		t.Errorf("重启后 self 节点应为离线，online=%d", self.Online)
	}
	if n, _ := db.GetNode(d, remote.ID); n.Online != 0 {
		t.Errorf("重启后远程节点应为离线，online=%d", n.Online)
	}
}

// 本机入口的 socket 只有属主可连（它不验 token，文件权限就是认证）。
func TestListenLocalAgentSocketIsOwnerOnly(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "panel.sock")
	l, err := ListenLocalAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket 权限应为 0600，实际 %o", perm)
	}
}

// 上一个面板进程异常退出留下的 socket 文件要能被接管；但另一个正在服务的面板进程
// 的 socket 不能抢——抢过来之后本机 daemon 下次重连会连到这个进程，被它的规则集
// 整体替换。
func TestListenLocalAgentTakesOverStaleButNotLiveSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "panel.sock")

	// 残留：监听过、进程没了、文件还在。
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("前置条件：残留的 socket 文件应存在: %v", err)
	}
	live, err := ListenLocalAgent(path)
	if err != nil {
		t.Fatalf("残留的 socket 应被接管: %v", err)
	}
	defer live.Close()

	// 在用：live 正在监听。
	if l, err := ListenLocalAgent(path); err == nil {
		l.Close()
		t.Fatal("另一个进程正在监听的 socket 不应被抢占")
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("被拒绝的抢占不应破坏原监听: %v", err)
	}
	c.Close()
}

// shortTempDir 返回一个路径很短的临时目录：unix socket 路径有长度上限（macOS 上
// 104 字节），t.TempDir() 的路径加上文件名容易超。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nftf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
