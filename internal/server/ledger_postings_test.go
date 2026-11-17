package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// ledgerFixture 是一份覆盖各种记账位置的现场：
//
//	E1 双向 倍率 2.0        E2 单向 倍率 0.5
//	M1 单向 倍率 3.0 ┐      P1 双向 ┐
//	M2 双向         ┘ C     P2 单向 ┘ CE（组合入口，倍率 3.0）
//	                 （组合中转层，倍率 7.0）
//
// 用户 U 的费率 1.5，管理员 A 的费率 1.0。
//
//	R1  U  E1                 tcp      出口 = U 在册的落地出口
//	R2  U  E2 → C(M1 → M2)    tcp      出口 = U 不在册（present=0）的落地出口
//	R3  U  CE(P1 → P2)        tcp+udp  出口 = U 在册的落地出口
//	R4  A  E1                 tcp      出口 = A 在册的落地出口
//	R5  无属主 E1              tcp
type ledgerFixture struct {
	d   *sql.DB
	s   *Server
	adm *http.Cookie

	u, a                          int64
	e1, e2, m1, m2, c, p1, p2, ce int64
	r1, r2, r3, r4, r5            int64
	hops                          map[int64][]*db.RuleHop
	touchedMu                     sync.Mutex
	touched                       map[int64]bool
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	f := &ledgerFixture{d: openDB(t), hops: map[int64][]*db.RuleHop{}, touched: map[int64]bool{}}
	d := f.d

	physical := func(name, relay string, mult float64, uni bool) int64 {
		t.Helper()
		n, err := db.CreateNode(d, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateNodeRelayHost(d, n.ID, relay); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateNodeRateMultiplier(d, n.ID, mult); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateNodeUnidirectional(d, n.ID, uni); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	f.e1 = physical("E1", "10.0.0.1", 2.0, false)
	f.e2 = physical("E2", "10.0.0.2", 0.5, true)
	f.m1 = physical("M1", "10.0.0.3", 3.0, true)
	f.m2 = physical("M2", "10.0.0.4", 1.0, false)
	f.p1 = physical("P1", "10.0.0.5", 1.0, false)
	f.p2 = physical("P2", "10.0.0.6", 1.0, true)
	f.c = makeComposite(t, d, "C", f.m1, f.m2).ID
	f.ce = makeComposite(t, d, "CE", f.p1, f.p2).ID
	if err := db.UpdateNodeRateMultiplier(d, f.c, 7.0); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeRateMultiplier(d, f.ce, 3.0); err != nil {
		t.Fatal(err)
	}

	f.u, _ = loginAsUser(t, d, 100)
	if _, err := d.Exec(`UPDATE users SET billing_rate=1.5 WHERE id=?`, f.u); err != nil {
		t.Fatal(err)
	}
	f.adm = loginAsAdmin(t, d)
	admin, err := db.GetUserByUsername(d, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	f.a = admin.ID
	// M2 与 P2 上的授权只为钉住“非段首跳不记授权账”：即使有授权行也不增长。
	for _, n := range []int64{f.e1, f.e2, f.c, f.ce, f.m2, f.p2} {
		if err := db.GrantNode(d, f.u, n, 10, 0); err != nil {
			t.Fatal(err)
		}
	}
	seedLandingExit(t, d, f.u, "8.8.8.8", 443, 0, 0)
	seedLandingExit(t, d, f.u, "9.9.9.9", 443, 0, 0)
	if _, err := d.Exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=? AND host='9.9.9.9'`, f.u); err != nil {
		t.Fatal(err)
	}
	seedLandingExit(t, d, f.u, "7.7.7.7", 443, 0, 0)
	seedLandingExit(t, d, f.a, "6.6.6.6", 443, 0, 0)

	owner := func(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }
	build := func(name string, ownerID, entry int64, proto, exitHost string, via []int64, hops []db.HopInput) int64 {
		t.Helper()
		rl := &db.Rule{NodeID: entry, OwnerID: owner(ownerID), Name: name, Proto: proto,
			ExitHost: exitHost, ExitPort: 443, ViaNodeIDs: via}
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.CreateRule(tx, rl)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		rl.ID = id
		if _, _, _, err := db.RegenerateRule(tx, rl, hops, nil); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		hs, err := db.ListRuleHops(d, id)
		if err != nil || len(hs) != len(hops) {
			t.Fatalf("rule %s: hops=%d err=%v", name, len(hs), err)
		}
		f.hops[id] = hs
		return id
	}
	f.r1 = build("R1", f.u, f.e1, "tcp", "8.8.8.8", nil, []db.HopInput{{NodeID: f.e1, Mode: "kernel", ViaNodeID: f.e1}})
	f.r2 = build("R2", f.u, f.e2, "tcp", "9.9.9.9", []int64{f.c}, []db.HopInput{
		{NodeID: f.e2, Mode: "userspace", ViaNodeID: f.e2},
		{NodeID: f.m1, Mode: "userspace", ViaNodeID: f.c},
		{NodeID: f.m2, Mode: "kernel", ViaNodeID: f.c},
	})
	f.r3 = build("R3", f.u, f.ce, "tcp+udp", "7.7.7.7", nil, []db.HopInput{
		{NodeID: f.p1, Mode: "userspace", ViaNodeID: f.ce},
		{NodeID: f.p2, Mode: "kernel", ViaNodeID: f.ce},
	})
	f.r4 = build("R4", f.a, f.e1, "tcp", "6.6.6.6", nil, []db.HopInput{{NodeID: f.e1, Mode: "kernel", ViaNodeID: f.e1}})
	f.r5 = build("R5", 0, f.e1, "tcp", "5.5.5.5", nil, []db.HopInput{{NodeID: f.e1, Mode: "kernel", ViaNodeID: f.e1}})

	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	s.Hub.OnTrafficUpdate = func(userID int64) {
		f.touchedMu.Lock()
		f.touched[userID] = true
		f.touchedMu.Unlock()
	}
	return f
}

func (f *ledgerFixture) port(rule int64, pos int) int { return f.hops[rule][pos].ListenPort }

// unmatchedPort 是 E1 上没有任何规则跳占用的端口：它的样本只计入节点的原始流量。
const unmatchedPort = 1

// report 让每台物理节点各报一批计数，各自只报落在自己身上的跳。
func (f *ledgerFixture) report() {
	h := f.s.Hub
	h.applyCounters(f.e1, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: f.port(f.r1, 0), BytesUp: 300, BytesDown: 700},
		{Proto: "tcp", ListenPort: f.port(f.r4, 0), BytesUp: 50, BytesDown: 50},
		{Proto: "tcp", ListenPort: f.port(f.r5, 0), BytesUp: 10, BytesDown: 30},
		{Proto: "tcp", ListenPort: unmatchedPort, BytesUp: 7, BytesDown: 3},
	})
	h.applyCounters(f.e2, []wsproto.CounterSample{{Proto: "tcp", ListenPort: f.port(f.r2, 0), BytesUp: 901, BytesDown: 100}})
	h.applyCounters(f.m1, []wsproto.CounterSample{{Proto: "tcp", ListenPort: f.port(f.r2, 1), BytesUp: 901, BytesDown: 100}})
	h.applyCounters(f.m2, []wsproto.CounterSample{{Proto: "tcp", ListenPort: f.port(f.r2, 2), BytesUp: 901, BytesDown: 100}})
	// P1 的入口跳是 userspace：tcp+udp 被拆成 tcp 用户态与 udp 内核两个样本，扇入同一跳。
	h.applyCounters(f.p1, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: f.port(f.r3, 0), BytesUp: 100, BytesDown: 201},
		{Proto: "udp", ListenPort: f.port(f.r3, 0), BytesUp: 50, BytesDown: 51},
	})
	// P2 是 kernel：tcp+udp 以一个聚合样本上报。它报的字节与 P1 不同，落地出口账记的
	// 是哪一跳的样本才看得出来。
	h.applyCounters(f.p2, []wsproto.CounterSample{{Proto: "tcp+udp", ListenPort: f.port(f.r3, 1), BytesUp: 150, BytesDown: 260}})
}

func (f *ledgerFixture) one(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := f.d.QueryRow(q, args...).Scan(&v); err != nil && err != sql.ErrNoRows {
		t.Fatalf("%s %v: %v", q, args, err)
	}
	return v.Int64
}

func (f *ledgerFixture) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(f.adm)
	rec := httptest.NewRecorder()
	f.s.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

// 一组计数批次在每本账上留下的增量。三种字节单位：
//
//	原始 = 上行 + 下行
//	折算 = 单向节点取 max(上, 下)，双向节点等于原始
//	加权 = 折算 × 规则逻辑入口节点的倍率 × 属主费率，每个样本各自四舍五入
//
// 期望值都从样本手算，算式写在每一行旁边。
func TestCounterBatchesPostEveryLedger(t *testing.T) {
	f := newLedgerFixture(t)
	f.report()

	type row struct {
		name string
		got  int64
		want int64
	}
	rows := []row{
		// 节点原始流量：每个样本的上行 + 下行，含没有匹配到规则跳的样本；组合不是物理节点，没有这本账。
		{"E1 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.e1), 1000 + 100 + 40 + 10},
		{"E2 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.e2), 1001},
		{"M1 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.m1), 1001},
		{"M2 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.m2), 1001},
		{"P1 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.p1), 301 + 101},
		{"P2 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.p2), 410},
		{"C 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.c), 0},
		{"CE 原始", f.one(t, `SELECT raw_bytes FROM node_raw_traffic WHERE node_id=?`, f.ce), 0},

		// 用户全局已用量：加权，只在入口跳记一次。
		// U = R1 1000×2.0×1.5 + R2 round(901×0.5×1.5) + R3 round(301×3.0×1.5)+round(101×3.0×1.5)
		{"U 全局", f.one(t, `SELECT traffic_used_bytes FROM users WHERE id=?`, f.u), 3000 + 676 + 1355 + 455},
		// A = R4 100×2.0×1.0
		{"A 全局", f.one(t, `SELECT traffic_used_bytes FROM users WHERE id=?`, f.a), 200},

		// 授权已用量：折算、不加权，每个逻辑段只在段首跳记一次，记在段的逻辑节点上。
		{"U@E1 授权", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.e1), 1000},
		{"U@E2 授权（单向取 max）", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.e2), 901},
		{"U@C 授权（段首 M1 单向取 max）", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.c), 901},
		{"U@CE 授权（tcp 与 udp 扇入）", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.ce), 402},
		{"U@M2 授权（非段首）", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.m2), 0},
		{"U@P2 授权（非段首）", f.one(t, `SELECT traffic_used_bytes FROM user_nodes WHERE user_id=? AND node_id=?`, f.u, f.p2), 0},

		// 落地出口账：原始、不折算不加权，只在末跳记，且出口须在属主名下在册。
		{"U 8.8.8.8 合计", f.one(t, `SELECT used_bytes FROM user_landing_exits WHERE user_id=? AND host='8.8.8.8'`, f.u), 1000},
		{"U 8.8.8.8 上行", f.one(t, `SELECT used_bytes_up FROM user_landing_exits WHERE user_id=? AND host='8.8.8.8'`, f.u), 300},
		{"U 8.8.8.8 下行", f.one(t, `SELECT used_bytes_down FROM user_landing_exits WHERE user_id=? AND host='8.8.8.8'`, f.u), 700},
		{"U 9.9.9.9 不在册", f.one(t, `SELECT used_bytes FROM user_landing_exits WHERE user_id=? AND host='9.9.9.9'`, f.u), 0},
		{"U 7.7.7.7 合计（末跳 P2 单向也取原始）", f.one(t, `SELECT used_bytes FROM user_landing_exits WHERE user_id=? AND host='7.7.7.7'`, f.u), 410},
		{"U 7.7.7.7 上行", f.one(t, `SELECT used_bytes_up FROM user_landing_exits WHERE user_id=? AND host='7.7.7.7'`, f.u), 150},
		{"U 7.7.7.7 下行", f.one(t, `SELECT used_bytes_down FROM user_landing_exits WHERE user_id=? AND host='7.7.7.7'`, f.u), 260},
		{"A 6.6.6.6 合计", f.one(t, `SELECT used_bytes FROM user_landing_exits WHERE user_id=? AND host='6.6.6.6'`, f.a), 100},

		// 逐跳累计：每一跳都记各自物理节点的折算，入口跳也一样。
		{"R1#0", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r1][0].ID), 1000},
		{"R2#0（E2 单向）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r2][0].ID), 901},
		{"R2#1（M1 单向）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r2][1].ID), 901},
		{"R2#2（M2 双向）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r2][2].ID), 1001},
		{"R3#0（tcp 与 udp 扇入）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r3][0].ID), 301 + 101},
		{"R3#1（P2 单向）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r3][1].ID), 260},
		{"R4#0", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r4][0].ID), 100},
		{"R5#0（无属主）", f.one(t, `SELECT total_bytes FROM rule_hops WHERE id=?`, f.hops[f.r5][0].ID), 40},

		// 规则账：规则的加权流量，只在入口跳记；无属主的规则不向任何人计价，倍率与费率按 1。
		{"R1 规则账", f.one(t, `SELECT weighted_bytes FROM rules WHERE id=?`, f.r1), 3000},
		{"R2 规则账", f.one(t, `SELECT weighted_bytes FROM rules WHERE id=?`, f.r2), 676},
		{"R3 规则账", f.one(t, `SELECT weighted_bytes FROM rules WHERE id=?`, f.r3), 1355 + 455},
		{"R4 规则账", f.one(t, `SELECT weighted_bytes FROM rules WHERE id=?`, f.r4), 200},
		{"R5 规则账（无属主）", f.one(t, `SELECT weighted_bytes FROM rules WHERE id=?`, f.r5), 40},
	}

	// 对外 JSON：rule.total_bytes 是规则账，hops[].total_bytes 是逐跳账。
	for _, rc := range []struct {
		rule int64
		want int64
		hops []int64
	}{
		{f.r1, 3000, []int64{1000}},
		{f.r2, 676, []int64{901, 901, 1001}},
		{f.r3, 1355 + 455, []int64{402, 260}},
		{f.r4, 200, []int64{100}},
		{f.r5, 40, []int64{40}},
	} {
		var body struct {
			Rule struct {
				TotalBytes int64 `json:"total_bytes"`
			} `json:"rule"`
			Hops []struct {
				TotalBytes int64 `json:"total_bytes"`
			} `json:"hops"`
		}
		f.getJSON(t, "/api/rules/"+strconv.FormatInt(rc.rule, 10), &body)
		rows = append(rows, row{fmt.Sprintf("规则 %d 的 total_bytes", rc.rule), body.Rule.TotalBytes, rc.want})
		if len(body.Hops) != len(rc.hops) {
			t.Fatalf("规则 %d 的 hops：%d 跳，期望 %d", rc.rule, len(body.Hops), len(rc.hops))
		}
		for i, want := range rc.hops {
			rows = append(rows, row{fmt.Sprintf("规则 %d 第 %d 跳的 total_bytes", rc.rule, i), body.Hops[i].TotalBytes, want})
		}
	}
	var dash struct {
		TotalBytes int64 `json:"total_bytes"`
	}
	f.getJSON(t, "/api/dashboard", &dash)
	rows = append(rows, row{"仪表盘 total_bytes", dash.TotalBytes, 3000 + 676 + 1355 + 455 + 200 + 40})

	for _, r := range rows {
		if r.got != r.want {
			t.Errorf("%s: got %d, want %d", r.name, r.got, r.want)
		}
	}
}

// 规则的加权流量是规则行上的一列，只由计数入账与用户重置写：改表头
// （UpdateRuleHeader）与重建链（RegenerateRule 把一条规则的跳整体删除重建）之后它
// 都不变，并且仍与属主的全局已用量对得上——两边记的是同一批入口跳样本的同一个
// 加权值。
func TestRuleTrafficSurvivesHeaderEditAndRegeneration(t *testing.T) {
	f := newLedgerFixture(t)
	f.report()

	check := func(after string) {
		t.Helper()
		var body struct {
			Rule struct {
				TotalBytes int64 `json:"total_bytes"`
			} `json:"rule"`
		}
		f.getJSON(t, "/api/rules/"+strconv.FormatInt(f.r2, 10), &body)
		if body.Rule.TotalBytes != 676 {
			t.Fatalf("%s之后规则流量应仍为 676，实际 %d", after, body.Rule.TotalBytes)
		}
		sum := f.one(t, `SELECT SUM(weighted_bytes) FROM rules WHERE owner_id=?`, f.u)
		used := f.one(t, `SELECT traffic_used_bytes FROM users WHERE id=?`, f.u)
		if sum != used {
			t.Fatalf("%s之后 U 名下规则流量之和 %d 应等于 U 的全局已用量 %d", after, sum, used)
		}
	}
	inTx := func(fn func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := f.d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	rl, err := db.GetRule(f.d, f.r2)
	if err != nil {
		t.Fatal(err)
	}
	rl.Name = "R2-renamed"
	inTx(func(tx *sql.Tx) error { return db.UpdateRuleHeader(tx, rl) })
	check("改表头")

	inTx(func(tx *sql.Tx) error {
		_, _, _, err := db.RegenerateRule(tx, rl, []db.HopInput{
			{NodeID: f.e2, Mode: "userspace", ViaNodeID: f.e2},
			{NodeID: f.m1, Mode: "userspace", ViaNodeID: f.c},
			{NodeID: f.m2, Mode: "kernel", ViaNodeID: f.c},
		}, nil)
		return err
	})
	check("重建链")
}

// 配额回调按属主触发，一批里一个属主只触发一次：同一台物理节点上的两跳分属两个
// 逻辑段（单点 N 与只含 N 的组合 K），这一批记了他两本授权账、一本落地出口账，回调
// 仍只来一次。
func TestQuotaCallbackOncePerOwnerPerBatch(t *testing.T) {
	d := openDB(t)
	uid, _ := loginAsUser(t, d, 10)
	n, _ := db.CreateNode(d, "N", "", "")
	_ = db.UpdateNodeRelayHost(d, n.ID, "10.0.1.1")
	k := makeComposite(t, d, "K", n.ID)
	_ = db.GrantNode(d, uid, n.ID, 5, 0)
	_ = db.GrantNode(d, uid, k.ID, 5, 0)
	seedLandingExit(t, d, uid, "8.8.8.8", 443, 0, 0)
	plain := createTestRuleDirectNode(t, d, uid, n.ID)
	viaK := &db.Rule{NodeID: k.ID, OwnerID: sql.NullInt64{Int64: uid, Valid: true}, Name: "via-k", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	tx, _ := d.Begin()
	id, err := db.CreateRule(tx, viaK)
	if err != nil {
		t.Fatal(err)
	}
	viaK.ID = id
	if _, _, _, err := db.RegenerateRule(tx, viaK, []db.HopInput{{NodeID: n.ID, Mode: "kernel", ViaNodeID: k.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(d)
	calls := make(chan int64, 8)
	hub.OnTrafficUpdate = func(userID int64) { calls <- userID }
	hub.applyCounters(n.ID, []wsproto.CounterSample{
		{Proto: "tcp", ListenPort: getHopPort(t, d, plain, n.ID), BytesUp: 10, BytesDown: 10},
		{Proto: "tcp", ListenPort: getHopPort(t, d, viaK.ID, n.ID), BytesUp: 20, BytesDown: 20},
	})

	for _, node := range []int64{n.ID, k.ID} {
		if g, _ := db.GetNodeGrant(d, uid, node); g.TrafficUsedBytes == 0 {
			t.Fatalf("前置条件：这一批应记到授权 %d 上", node)
		}
	}
	select {
	case got := <-calls:
		if got != uid {
			t.Fatalf("回调的属主 = %d，期望 %d", got, uid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("配额回调没有触发")
	}
	select {
	case extra := <-calls:
		t.Fatalf("一批里同一个属主的回调触发了不止一次（又来了 %d）", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// 一批计数入账后，配额回调对每个配额账有增长的属主各触发；无属主的规则不触发。
// 速度缓存按规则属主归属每个样本，匹配不到规则跳或无属主的样本归属 0。
func TestCounterBatchesAttributeOwners(t *testing.T) {
	f := newLedgerFixture(t)
	f.report()

	waitFor(t, "配额回调应对 U 与 A 都触发", func() bool {
		f.touchedMu.Lock()
		defer f.touchedMu.Unlock()
		return f.touched[f.u] && f.touched[f.a]
	})
	f.touchedMu.Lock()
	var users []int64
	for u := range f.touched {
		users = append(users, u)
	}
	f.touchedMu.Unlock()
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })
	if len(users) != 2 {
		t.Fatalf("配额回调只应对 U(%d) 与 A(%d) 触发，实际 %v", f.u, f.a, users)
	}

	sc := f.s.Hub.speedCache
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	owner := func(node int64, proto string, port int) int64 {
		hs := sc.nodes[node].hops[proto+"/"+strconv.Itoa(port)]
		if hs == nil {
			t.Fatalf("速度缓存缺少 node %d 的 %s/%d", node, proto, port)
		}
		return hs.ownerID
	}
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"R1", owner(f.e1, "tcp", f.port(f.r1, 0)), f.u},
		{"R4", owner(f.e1, "tcp", f.port(f.r4, 0)), f.a},
		{"R5 无属主", owner(f.e1, "tcp", f.port(f.r5, 0)), 0},
		{"未匹配", owner(f.e1, "tcp", unmatchedPort), 0},
		{"R3 udp 扇入", owner(f.p1, "udp", f.port(f.r3, 0)), f.u},
		{"R3 聚合样本", owner(f.p2, "tcp+udp", f.port(f.r3, 1)), f.u},
	} {
		if c.got != c.want {
			t.Errorf("%s 的速度归属：got %d, want %d", c.name, c.got, c.want)
		}
	}
}
