package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 改动落库之后要经一次重新下发才到节点上，要通知哪些节点是从 rule_hops 读出来的。
// 读不出来时改动已经保存、却没有任何节点知道，接口必须回答 500 并说明“已保存，但
// 没能确定……”，不能按“没有节点需要通知”回 200。
//
// 表里每个接口的写入都不碰 rule_hops，所以丢掉这张表只让“读出要通知谁”这一步失败；
// 对照组（表都在）回 200，说明 500 不是请求本身有问题。重置用户流量
// （/users/{id}/reset-traffic）不在表里：它的写入本身要清 rule_hops 上的逐跳流量，
// 丢表分不开这两步。设置中继地址（/nodes/{id}/relay-host）也不在表里：它把改地址与
// 重新生成引用它的规则放在一个事务里（db.SetNodeRelayHostAndRewire），读不出要重接
// 哪些规则时地址根本不保存，没有“已保存但……”这种状态。
func TestSavedChangeReportsWhenItsNodesCannotBeListed(t *testing.T) {
	// 落地出口的两个接口要先有一个在册的出口：不在册的残留行不在下发排除的范围里，
	// 改它不需要通知任何节点。
	const exitHost, exitPort = "exit.example", 443
	presentExit := func(t *testing.T, d *sql.DB, uid int64) {
		t.Helper()
		if _, err := d.Exec(`INSERT INTO user_landing_exits(user_id, host, port, name, protocol, uri, present, updated_at)
			VALUES (?,?,?,?,?,?,1,1)`, uid, exitHost, exitPort, "exit", "trojan", "trojan://x@exit.example:443"); err != nil {
			t.Fatal(err)
		}
	}
	calls := []struct {
		name, method, url string
		body              map[string]any
		setup             func(t *testing.T, d *sql.DB, uid int64)
	}{
		{"设置全局流量配额", "POST", "/api/users/%[1]d/quota", map[string]any{"traffic_quota_bytes": 1 << 30}, nil},
		{"设置到期时间", "POST", "/api/users/%[1]d/expiry", map[string]any{"expires_at": "2099-01-01"}, nil},
		{"授权节点", "POST", "/api/users/%[1]d/grants", map[string]any{"node_id": "NODE", "max_forwards": 5}, nil},
		{"设置授权流量配额", "POST", "/api/users/%[1]d/nodes/%[2]d/quota", map[string]any{"traffic_quota_bytes": 1 << 30}, nil},
		{"设置授权限速", "POST", "/api/users/%[1]d/nodes/%[2]d/rate-limit", map[string]any{"rate_limit_mbytes": 10}, nil},
		{"重置授权流量", "POST", "/api/users/%[1]d/nodes/%[2]d/reset-traffic", nil, nil},
		{"启用/停用账号", "POST", "/api/users/%[1]d/toggle", nil, nil},
		{"修改用户资料", "PATCH", "/api/users/%[1]d/profile", map[string]any{"max_forwards": 10}, nil},
		{"节点启停", "POST", "/api/nodes/%[2]d/toggle", nil, nil},
		// 名单里是已经持有授权的用户：授权被覆盖写，经过它的规则要重新下发。
		{"节点授权名单", "POST", "/api/nodes/%[2]d/grants/batch", map[string]any{"add_user_ids": "USERS", "max_forwards": 5}, nil},
		{"设置落地出口流量配额", "POST", "/api/users/%[1]d/landing-exits/quota",
			map[string]any{"host": exitHost, "port": exitPort, "quota_bytes": 1 << 20}, presentExit},
		{"重置落地出口流量", "POST", "/api/users/%[1]d/landing-exits/reset",
			map[string]any{"host": exitHost, "port": exitPort}, presentExit},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			for _, lost := range []bool{false, true} {
				d := openDB(t)
				admin := loginAsAdmin(t, d)
				uid, _ := loginAsUser(t, d, 10)
				node := grantedNode(t, d, "edge", uid, 5)
				if c.setup != nil {
					c.setup(t, d, uid)
				}
				s, _ := New(d)
				body := map[string]any{}
				for k, v := range c.body {
					switch v {
					case "NODE":
						v = node.ID
					case "USERS":
						v = []int64{uid}
					}
					body[k] = v
				}
				if lost {
					loseTable(t, d, "rule_hops")
				}
				rec := cookieDo(t, s, admin, c.method, fmt.Sprintf(c.url, uid, node.ID), body)
				if !lost {
					if rec.Code != http.StatusOK {
						t.Fatalf("对照：表都在时应回 200，实际 %d: %s", rec.Code, rec.Body.String())
					}
					continue
				}
				if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "但没能确定需要") {
					t.Fatalf("rule_hops 读不出来：应回 500 并说明改动已保存，实际 %d: %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}
