package rulechange

import (
	"testing"

	"nft-forward/internal/db"
)

// loseTable 让碰到这张表的语句全部失败，库的其余部分照常可用。
func loseTable(t *testing.T, f *fixture, table string) {
	t.Helper()
	if _, err := f.d.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_gone`); err != nil {
		t.Fatalf("改名 %s: %v", table, err)
	}
}

// 下面每个检查都凭一次查询决定一个变更能不能继续。查询失败时检查必须报告失败（类别
// Internal），不能按“什么都没查到”作答——那样的回答各自意味着：没有授权行是无权
// （Forbidden，把库的故障说成发起方的问题），数到 0 条是没到上限（放行），没有授权
// 覆盖是资格不收窄（放行），拓扑里没有这个节点是参数有误（Invalid）。
func TestGuardsReportAFailedLookupInsteadOfPassing(t *testing.T) {
	guards := []struct {
		name  string
		table string
		check func(t *testing.T, f *fixture, u *db.User, node int64) *Error
	}{
		{"节点授权", "user_nodes", func(t *testing.T, f *fixture, u *db.User, node int64) *Error {
			_, cerr := f.c.access(u, node, nil)
			return cerr
		}},
		{"账号的条数上限", "rules", func(t *testing.T, f *fixture, u *db.User, node int64) *Error {
			return withinCaps(f.d, u, usage{rules: 1})
		}},
		{"授权的条数上限", "rules", func(t *testing.T, f *fixture, u *db.User, node int64) *Error {
			grant, cerr := f.c.access(u, node, nil)
			if cerr != nil {
				t.Fatalf("前置条件：授权应能读到: %s", cerr.Msg)
			}
			var use usage
			use.add(node, grant)
			return withinNodeCaps(f.d, u, use)
		}},
		{"授权对入口与中转资格的覆盖", "user_nodes", func(t *testing.T, f *fixture, u *db.User, node int64) *Error {
			_, cerr := f.c.ownerSeats(u)
			return cerr
		}},
		{"推导链用的拓扑", "node_hops", func(t *testing.T, f *fixture, u *db.User, node int64) *Error {
			_, cerr := f.c.Create(Actor{User: u}, fields("r"), Path{Entry: node}, true)
			return cerr
		}},
	}
	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			f := newFixture(t)
			u := f.user("user", 10)
			node := f.node("edge")
			f.grant(u, node, 5)
			if cerr := g.check(t, f, u, node); cerr != nil {
				t.Fatalf("对照：表都在时应通过，实际被拒（类别 %d）: %s", cerr.Kind, cerr.Msg)
			}
			loseTable(t, f, g.table)
			cerr := g.check(t, f, u, node)
			if cerr == nil {
				t.Fatalf("%s 读不出来，检查却通过了", g.table)
			}
			if cerr.Kind != Internal {
				t.Fatalf("%s 读不出来，检查应报 Internal，实际类别 %d: %s", g.table, cerr.Kind, cerr.Msg)
			}
		})
	}
}
