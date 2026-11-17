package db

import (
	"database/sql"
	"reflect"
	"testing"
)

// 下发给节点的规则跳有确定的顺序，不随跳行的物理位置变。
//
// 同一节点上 tcp 与 udp 可以各占同一个监听端口（OccupiedPortsOnNode 按协议的命名空间
// 判占用），而 RegenerateRule 每次都把规则的跳行删掉重插。顺序要是跟着行的物理位置
// 走，重建其中一条规则就会把两行对调：面板给载荷算的 rev 变了，节点按下标比较新旧
// 规则集的数据面（nft.SameDataplane）也会当成数据面变了。
func TestPushOrderDoesNotDependOnWhenAHopRowWasWritten(t *testing.T) {
	d := openTestDB(t)
	nid := createTestNode(t, d, "edge")
	if err := UpdateNodeRelayHost(d, nid, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	const port = 12000
	regenerate := func(rl *Rule) {
		t.Helper()
		tx, err := d.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if rl.ID == 0 {
			if rl.ID, err = CreateRule(tx, rl); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, _, err := RegenerateRule(tx, rl, []HopInput{{NodeID: nid, DesiredPort: port}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(proto string) *Rule {
		rl := &Rule{NodeID: nid, OwnerID: sql.NullInt64{}, Name: proto, Proto: proto, ExitHost: "198.51.100.9", ExitPort: 9000}
		regenerate(rl)
		return rl
	}
	pushed := func() []string {
		t.Helper()
		hops, err := ActiveRuleHopsForPush(d, nid)
		if err != nil {
			t.Fatal(err)
		}
		var protos []string
		for _, h := range hops {
			if h.ListenPort != port {
				t.Fatalf("前置条件：两条规则都应监听 %d，实际有一跳监听 %d", port, h.ListenPort)
			}
			protos = append(protos, h.Proto)
		}
		return protos
	}

	// udp 先建、tcp 后建，再把先建的那条重建一遍：三个时刻跳行的物理顺序各不相同。
	udp := rule("udp")
	tcp := rule("tcp")
	want := []string{"tcp", "udp"}
	if got := pushed(); !reflect.DeepEqual(got, want) {
		t.Errorf("udp 先建、tcp 后建：下发顺序应为 %v，实际 %v", want, got)
	}
	regenerate(udp)
	if got := pushed(); !reflect.DeepEqual(got, want) {
		t.Errorf("重建 udp 那条之后：下发顺序应为 %v，实际 %v", want, got)
	}
	regenerate(tcp)
	if got := pushed(); !reflect.DeepEqual(got, want) {
		t.Errorf("再重建 tcp 那条之后：下发顺序应为 %v，实际 %v", want, got)
	}
}
