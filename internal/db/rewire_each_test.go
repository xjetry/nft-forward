package db

import (
	"database/sql"
	"errors"
	"testing"
)

// twoHopRule 建一条 entry → exit 的两跳规则并生成它的跳。
func twoHopRule(t *testing.T, d *sql.DB, name string, entry, exit int64) int64 {
	t.Helper()
	r := &Rule{NodeID: entry, Name: name, Proto: "tcp", ExitHost: "203.0.113.9", ExitPort: 443, ViaNodeIDs: []int64{exit}}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if r.ID, err = CreateRule(tx, r); err != nil {
		t.Fatal(err)
	}
	hops := []HopInput{{NodeID: entry, Mode: "userspace", ViaNodeID: entry}, {NodeID: exit, Mode: "userspace", ViaNodeID: exit}}
	if _, _, _, err := RegenerateRule(tx, r, hops, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func entryTarget(t *testing.T, d *sql.DB, ruleID int64) string {
	t.Helper()
	var host string
	if err := d.QueryRow(`SELECT target_host FROM rule_hops WHERE rule_id=? AND position=0`, ruleID).Scan(&host); err != nil {
		t.Fatal(err)
	}
	return host
}

// RewireEachRule 给“改动已经落库、没有拒绝余地”的调用方用（节点自己上报的地址）：每条
// 规则各自一个事务，重生成不了的那条保持原样、报在错误里，其余的照常重新生成。
//
// 两条规则的末跳都在 exit 上，入口跳的目标是建规则时抄下的 exit 的地址。exit 换了地址
// 之后两条都要重接；其中 stuck 的入口节点没有数据面地址了，这条规则重生成不了（每一跳
// 所在的节点都要有地址）。stuck 排在前面：重生成不了的那条若拖住后面的，fine 就不会
// 跟上新地址。
func TestRewireEachRuleRewiresTheRestWhenOneRuleCannotBe(t *testing.T) {
	d := openTestDB(t)
	node := func(name, relay string) int64 {
		n, err := CreateNode(d, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := UpdateNodeRelayHost(d, n.ID, relay); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	entryA, entryB, exit := node("entry-a", "10.0.0.1"), node("entry-b", "10.0.0.2"), node("exit", "10.0.0.3")
	stuck := twoHopRule(t, d, "stuck", entryA, exit)
	fine := twoHopRule(t, d, "fine", entryB, exit)
	for _, id := range []int64{stuck, fine} {
		if got := entryTarget(t, d, id); got != "10.0.0.3" {
			t.Fatalf("前置条件：规则 %d 的入口跳应指向 exit 的旧地址，实际 %q", id, got)
		}
	}

	mustExec(t, d, `UPDATE nodes SET relay_host='10.0.9.9' WHERE id=?`, exit)
	mustExec(t, d, `UPDATE nodes SET relay_host='' WHERE id=?`, entryA)

	nodes, err := RewireEachRule(d, []int64{stuck, fine})

	var rerr *RewireError
	if !errors.As(err, &rerr) || rerr.RuleID != stuck || !IsChainError(err) {
		t.Fatalf("错误应是点名规则 %d 的 *RewireError，原因是一个 ChainError，实际 %v", stuck, err)
	}
	if got := entryTarget(t, d, fine); got != "10.0.9.9" {
		t.Errorf("fine 的入口跳应跟上 exit 的新地址，实际 %q", got)
	}
	if got := entryTarget(t, d, stuck); got != "10.0.0.3" {
		t.Errorf("stuck 没能重新生成，应保持原样，入口跳的目标却成了 %q", got)
	}
	seen := map[int64]bool{}
	for _, n := range nodes {
		seen[n] = true
	}
	if !seen[entryB] || seen[entryA] {
		t.Errorf("要重新下发的节点应含 fine 的入口 %d、不含 stuck 的入口 %d，实际 %v", entryB, entryA, nodes)
	}
}
