package accounting

import (
	"database/sql"
	"testing"

	"nft-forward/internal/db"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newUser(t *testing.T, d *sql.DB) int64 {
	t.Helper()
	id, err := db.CreateUser(d, "u-"+db.RandToken(4), "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newNode 建一台带中继地址的物理节点。
func newNode(t *testing.T, d *sql.DB, name, relay string) int64 {
	t.Helper()
	n, err := db.CreateNode(d, name+"-"+db.RandToken(4), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeRelayHost(d, n.ID, relay); err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// newComposite 建一个按顺序由 members 组成的组合节点。
func newComposite(t *testing.T, d *sql.DB, name string, members ...int64) int64 {
	t.Helper()
	c, err := db.CreateNode(d, name+"-"+db.RandToken(4), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	hops := make([]db.NodeHop, len(members))
	for i, m := range members {
		hops[i] = db.NodeHop{NodeID: c.ID, Position: i, HopNodeID: m, Mode: "userspace"}
	}
	if err := db.CreateNodeHops(d, c.ID, hops); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// newRule 建一条规则并按 hops 落它的物理链，返回规则 id 与按 position 排好的跳。
func newRule(t *testing.T, d *sql.DB, rl *db.Rule, hops []db.HopInput) (int64, []*db.RuleHop) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := db.CreateRule(tx, rl)
	if err != nil {
		t.Fatal(err)
	}
	rl.ID = id
	if _, _, _, err := db.RegenerateRule(tx, rl, hops, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rh, err := db.ListRuleHops(d, id)
	if err != nil {
		t.Fatal(err)
	}
	return id, rh
}

func owned(uid int64) sql.NullInt64 { return sql.NullInt64{Int64: uid, Valid: uid != 0} }

// seedExit 在用户名下放一个在册（present=1）的落地出口。
func seedExit(t *testing.T, d *sql.DB, uid int64, host string, port int) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO user_landing_exits(user_id, host, port, present) VALUES (?,?,?,1)`, uid, host, port); err != nil {
		t.Fatal(err)
	}
}

func one(t *testing.T, d *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := d.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s %v: %v", q, args, err)
	}
	return v
}
