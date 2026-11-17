package db

import (
	"testing"

	"nft-forward/internal/chain"
)

// 节点行与授权行上“可以占链里哪些位置”的那一列叫 seats。它原来叫 roles，与账号的
// 角色（users.role）撞名。迁移只改列名：两张表上每一行的值原样留下，旧名字不再存在；
// 授权行上的 0（沿用节点自己的）也原样留下。
func TestMigrationRenamesTheSeatColumns(t *testing.T) {
	const migration = "0056_node_seats.sql"
	d := openTestDB(t)
	tables := []string{"nodes", "user_nodes"}
	for _, table := range tables {
		if n := columnCount(t, d, table, "seats"); n != 1 {
			t.Fatalf("跑完全部迁移的库里 %s 应有 seats 列，实际 %d 个", table, n)
		}
		if n := columnCount(t, d, table, "roles"); n != 0 {
			t.Fatalf("跑完全部迁移的库里 %s 不应再有 roles 列，实际 %d 个", table, n)
		}
	}

	uid := createTestUser(t, d)
	viaOnly := createTestNode(t, d, "via-only")
	both := createTestNode(t, d, "both")
	if err := UpdateNodeSeats(d, viaOnly, chain.SeatVia); err != nil {
		t.Fatal(err)
	}
	for _, nid := range []int64{viaOnly, both} {
		if err := GrantNode(d, uid, nid, 5, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetGrantSeats(d, uid, viaOnly, chain.SeatEntry); err != nil {
		t.Fatal(err)
	}
	type row struct{ node, grant int }
	want := map[int64]row{viaOnly: {2, 1}, both: {3, 0}}
	stored := func(column string) map[int64]row {
		t.Helper()
		got := map[int64]row{}
		for nid := range want {
			var r row
			if err := d.QueryRow(`SELECT `+column+` FROM nodes WHERE id=?`, nid).Scan(&r.node); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT `+column+` FROM user_nodes WHERE user_id=? AND node_id=?`, uid, nid).Scan(&r.grant); err != nil {
				t.Fatal(err)
			}
			got[nid] = r
		}
		return got
	}
	same := func(got map[int64]row) bool {
		return len(got) == len(want) && got[viaOnly] == want[viaOnly] && got[both] == want[both]
	}

	// 回到迁移之前的形状，再重放这一个迁移。
	for _, table := range tables {
		mustExec(t, d, `ALTER TABLE `+table+` RENAME COLUMN seats TO roles`)
	}
	if got := stored("roles"); !same(got) {
		t.Fatalf("前置条件：改回旧列名后各行的值应是 %v，实际 %v", want, got)
	}
	res, err := d.Exec(`DELETE FROM schema_migrations WHERE version=?`, migration)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("前置条件：迁移 %s 应已登记，删除了 %d 行", migration, n)
	}
	if err := migrate(d); err != nil {
		t.Fatal(err)
	}

	for _, table := range tables {
		if n := columnCount(t, d, table, "roles"); n != 0 {
			t.Errorf("重放迁移后 %s 不应再有 roles 列，实际 %d 个", table, n)
		}
		if n := columnCount(t, d, table, "seats"); n != 1 {
			t.Errorf("重放迁移后 %s 应有 seats 列，实际 %d 个", table, n)
		}
	}
	if t.Failed() {
		return
	}
	if got := stored("seats"); !same(got) {
		t.Errorf("迁移只改列名，各行的值应仍是 %v，实际 %v", want, got)
	}

	// 读这两列的代码用的是新名字：节点行、单条授权、按用户列授权、按节点列授权四条
	// 读路径读出的都是写进去的值。
	n, err := GetNode(d, viaOnly)
	if err != nil || n.Seats != chain.SeatVia {
		t.Errorf("GetNode 应读出节点的位置 %b，实际 %+v，err=%v", chain.SeatVia, n, err)
	}
	g, err := GetNodeGrant(d, uid, viaOnly)
	if err != nil || g == nil || g.Seats != chain.SeatEntry {
		t.Errorf("GetNodeGrant 应读出授权上的覆盖 %b，实际 %+v，err=%v", chain.SeatEntry, g, err)
	}
	nodes, grants, err := ListNodesForUser(d, uid)
	if err != nil || len(nodes) != 2 || len(grants) != 2 {
		t.Fatalf("ListNodesForUser 应给出两个节点与两条授权，实际 %d、%d，err=%v", len(nodes), len(grants), err)
	}
	for i, node := range nodes {
		w := want[node.ID]
		if int(node.Seats) != w.node || int(grants[i].Seats) != w.grant {
			t.Errorf("ListNodesForUser：节点 %s 应是节点位置 %d、授权覆盖 %d，实际 %d、%d", node.Name, w.node, w.grant, node.Seats, grants[i].Seats)
		}
	}
	users, err := ListUsersForNode(d, viaOnly)
	if err != nil || len(users) != 1 || users[0].Seats != chain.SeatEntry {
		t.Errorf("ListUsersForNode 应读出授权上的覆盖 %b，实际 %+v，err=%v", chain.SeatEntry, users, err)
	}
}
