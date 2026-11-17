package accounting

import (
	"testing"

	"nft-forward/internal/db"
)

// LoadBook 从一跳的身份解析计价来源：是否单向看上报的物理节点，倍率看规则的逻辑
// 入口节点（组合入口就是组合自己，中转层与组合成员的倍率不算），费率看属主，落地
// 出口看属主名下在册的出口；位置（入口、末跳、段首）从同一规则的各跳算出。
//
//	规则：属主 U（费率 1.5），入口 CE = 组合(P1 → P2，倍率 3)，中转层 L = 组合(M1 → M2，倍率 7)
//	物理链：P1 → P2 → M1 → M2，via 依次是 CE CE L L
func TestLoadBookResolvesPricingAndPlaceFromTheHop(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	if _, err := d.Exec(`UPDATE users SET billing_rate=1.5 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	p1 := newNode(t, d, "p1", "10.0.0.1")
	p2 := newNode(t, d, "p2", "10.0.0.2")
	m1 := newNode(t, d, "m1", "10.0.0.3")
	m2 := newNode(t, d, "m2", "10.0.0.4")
	for _, n := range []int64{p1, m1, m2} {
		if err := db.UpdateNodeRateMultiplier(d, n, 5); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateNodeUnidirectional(d, p2, true); err != nil {
		t.Fatal(err)
	}
	ce := newComposite(t, d, "ce", p1, p2)
	layer := newComposite(t, d, "layer", m1, m2)
	_ = db.UpdateNodeRateMultiplier(d, ce, 3)
	_ = db.UpdateNodeRateMultiplier(d, layer, 7)
	seedExit(t, d, uid, "8.8.8.8", 443)
	_, hops := newRule(t, d, &db.Rule{NodeID: ce, OwnerID: owned(uid), Name: "r", Proto: "tcp",
		ExitHost: "8.8.8.8", ExitPort: 443, ViaNodeIDs: []int64{layer}}, []db.HopInput{
		{NodeID: p1, Mode: "userspace", ViaNodeID: ce},
		{NodeID: p2, Mode: "userspace", ViaNodeID: ce},
		{NodeID: m1, Mode: "userspace", ViaNodeID: layer},
		{NodeID: m2, Mode: "kernel", ViaNodeID: layer},
	})

	exit := &ExitKey{User: uid, Host: "8.8.8.8", Port: 443}
	for i, c := range []struct {
		node  int64
		place Place
		uni   bool
	}{
		{p1, Place{Entry: true, SegmentStart: true, Segment: ce}, false},
		{p2, Place{Segment: ce}, true},
		{m1, Place{SegmentStart: true, Segment: layer}, false},
		{m2, Place{Final: true, Segment: layer}, false},
	} {
		b, err := LoadBook(d, c.node)
		if err != nil {
			t.Fatal(err)
		}
		h := b.Hops[CounterKey{Proto: "tcp", Port: hops[i].ListenPort}]
		if h == nil {
			t.Fatalf("第 %d 跳：LoadBook 没有登记它的计数键", i)
		}
		want := Hop{ID: hops[i].ID, RuleID: hops[i].RuleID, Owner: uid, Place: c.place,
			Pricing: Pricing{Unidirectional: c.uni, Multiplier: 3, Rate: 1.5}, Exit: exit}
		if h.ID != want.ID || h.RuleID != want.RuleID || h.Owner != want.Owner || h.Place != want.Place ||
			h.Pricing != want.Pricing || h.Exit == nil || *h.Exit != *want.Exit {
			t.Errorf("第 %d 跳：\ngot  %+v exit=%v\nwant %+v exit=%v", i, *h, h.Exit, want, want.Exit)
		}
	}
}

// 无属主的规则按单价计价，也没有落地出口；出口不在册（present=0）时不登记出口。
func TestLoadBookOwnerlessAndAbsentExit(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	n := newNode(t, d, "n", "10.0.1.1")
	if err := db.UpdateNodeRateMultiplier(d, n, 4); err != nil {
		t.Fatal(err)
	}
	seedExit(t, d, uid, "9.9.9.9", 443)
	if _, err := d.Exec(`UPDATE user_landing_exits SET present=0 WHERE user_id=?`, uid); err != nil {
		t.Fatal(err)
	}
	_, absent := newRule(t, d, &db.Rule{NodeID: n, OwnerID: owned(uid), Name: "absent", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443},
		[]db.HopInput{{NodeID: n, Mode: "kernel"}})
	_, orphan := newRule(t, d, &db.Rule{NodeID: n, Name: "orphan", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443},
		[]db.HopInput{{NodeID: n, Mode: "kernel"}})

	b, err := LoadBook(d, n)
	if err != nil {
		t.Fatal(err)
	}
	if h := b.Hops[CounterKey{"tcp", absent[0].ListenPort}]; h == nil || h.Exit != nil || h.Pricing.Multiplier != 4 {
		t.Fatalf("出口不在册的规则：%+v", h)
	}
	h := b.Hops[CounterKey{"tcp", orphan[0].ListenPort}]
	if h == nil || h.Owner != 0 || h.Exit != nil || h.Pricing != (Pricing{Multiplier: 1, Rate: 1}) {
		t.Fatalf("无属主的规则应按单价、没有出口：%+v", h)
	}
}

// 一个 tcp+udp 的跳可能以一个聚合样本（内核模式）或拆开的 tcp 与 udp 两个样本
// （用户态模式下 forward.Partition 拆成 tcp 用户态 + udp 内核）上报，三个计数键都要
// 指向同一跳，字节才记进同一行。
func TestLoadBookFansTCPUDPHopInFromThreeKeys(t *testing.T) {
	d := openDB(t)
	n := newNode(t, d, "edge", "10.0.2.1")
	_, hops := newRule(t, d, &db.Rule{NodeID: n, Name: "r", Proto: "tcp+udp", ExitHost: "9.9.9.9", ExitPort: 443},
		[]db.HopInput{{NodeID: n, Mode: "userspace"}})
	b, err := LoadBook(d, n)
	if err != nil {
		t.Fatal(err)
	}
	port := hops[0].ListenPort
	hop := b.Hops[CounterKey{"tcp+udp", port}]
	if hop == nil || hop.ID != hops[0].ID {
		t.Fatalf("tcp+udp 键应指向这一跳，got %+v", hop)
	}
	for _, p := range []string{"tcp", "udp"} {
		if b.Hops[CounterKey{p, port}] != hop {
			t.Fatalf("%s 键应与 tcp+udp 键指向同一跳", p)
		}
	}
	if len(b.Hops) != 3 {
		t.Fatalf("一跳应只登记三个键，got %d", len(b.Hops))
	}
}
