package db

import (
	"database/sql"
	"reflect"
	"testing"
)

// hopBytes 是一条规则各跳的（节点, 逐跳流量），按位置排列。
func hopBytes(t *testing.T, d *sql.DB, ruleID int64) [][2]int64 {
	t.Helper()
	hops, err := ListRuleHops(d, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][2]int64, len(hops))
	for i, h := range hops {
		out[i] = [2]int64{h.NodeID, h.FoldedBytes}
	}
	return out
}

// regenerateOver 按 nodes 的顺序重新生成规则的跳，出口端口改成 exitPort；avoid 里的
// 节点不许再用所给的监听端口（RegenerateRule 的同名参数）。
func regenerateOver(t *testing.T, d *sql.DB, ruleID int64, exitPort int, avoid map[int64]int, nodes ...*Node) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r, err := GetRule(tx, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	r.ExitPort = exitPort
	hops := make([]HopInput, len(nodes))
	for i, n := range nodes {
		hops[i] = HopInput{NodeID: n.ID, Mode: "kernel", ViaNodeID: n.ID}
	}
	if _, _, _, err := RegenerateRule(tx, r, hops, avoid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// 重新生成一条规则的跳时，位置与物理节点都没变的跳保留它累计的逐跳流量：改出口、
// 改备注、给某一跳换监听端口、换掉链上别的节点，都不该让没动的那几跳的流量从头算。
// 换了节点的位置、以及节点没变但位置变了的跳，从 0 起。
func TestRegenerateRuleKeepsTheBytesOfHopsThatStayPut(t *testing.T) {
	d := openTestDB(t)
	a, b, c, x := relayNode(t, d, "a", "10.0.0.1"), relayNode(t, d, "b", "10.0.0.2"),
		relayNode(t, d, "c", "10.0.0.3"), relayNode(t, d, "x", "10.0.0.9")
	ruleID := chainRule(t, d, "kept", a, b, c)
	for pos, bytes := range []int64{100, 200, 300} {
		if _, err := d.Exec(`UPDATE rule_hops SET total_bytes=? WHERE rule_id=? AND position=?`, bytes, ruleID, pos); err != nil {
			t.Fatal(err)
		}
	}

	// 链不变，只改出口端口：每一跳都没动。
	regenerateOver(t, d, ruleID, 8443, nil, a, b, c)
	if got, want := hopBytes(t, d, ruleID), [][2]int64{{a.ID, 100}, {b.ID, 200}, {c.ID, 300}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("链没有变，各跳的流量应原样保留：得到 %v，期望 %v", got, want)
	}

	// 中间一跳换监听端口：节点与位置都没变，不算换跳。
	portOf := func(pos int) int {
		t.Helper()
		var p int
		if err := d.QueryRow(`SELECT listen_port FROM rule_hops WHERE rule_id=? AND position=?`, ruleID, pos).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := portOf(1)
	regenerateOver(t, d, ruleID, 8443, map[int64]int{b.ID: old}, a, b, c)
	if now := portOf(1); now == old {
		t.Fatalf("这一步要验证的是换了端口之后的流量，但端口没换（仍是 %d）", now)
	}
	if got, want := hopBytes(t, d, ruleID), [][2]int64{{a.ID, 100}, {b.ID, 200}, {c.ID, 300}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("只换了中间一跳的端口：得到 %v，期望 %v", got, want)
	}

	// 中间一跳换成另一台节点：换掉的那个位置从 0 起，两头不动。
	regenerateOver(t, d, ruleID, 8443, nil, a, x, c)
	if got, want := hopBytes(t, d, ruleID), [][2]int64{{a.ID, 100}, {x.ID, 0}, {c.ID, 300}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("只换了中间一跳：得到 %v，期望 %v", got, want)
	}

	// 去掉中间一跳：c 还在链上，但从第 2 位挪到了第 1 位，从 0 起；a 没动。
	regenerateOver(t, d, ruleID, 8443, nil, a, c)
	if got, want := hopBytes(t, d, ruleID), [][2]int64{{a.ID, 100}, {c.ID, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("去掉中间一跳：得到 %v，期望 %v", got, want)
	}
}
