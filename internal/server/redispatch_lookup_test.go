package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 改动落库之后要经一次重新下发（或重新接线）才到节点上，要通知哪些节点、重接哪些规则
// 是从 rule_hops 读出来的。读不出来时改动已经保存、却没有任何节点知道，接口必须回答
// 500 并说明“已保存，但没能确定……”，不能按“没有节点需要通知”回 200。
//
// 表里每个接口的写入都不碰 rule_hops，所以丢掉这张表只让“读出要通知谁”这一步失败；
// 对照组（表都在）回 200，说明 500 不是请求本身有问题。重置用户流量
// （/users/{id}/reset-traffic）不在表里：它的写入本身要清 rule_hops 上的逐跳流量，
// 丢表分不开这两步。
func TestSavedChangeReportsWhenItsNodesCannotBeListed(t *testing.T) {
	calls := []struct {
		name, method, url string
		body              map[string]any
	}{
		{"设置全局流量配额", "POST", "/api/users/%[1]d/quota", map[string]any{"traffic_quota_bytes": 1 << 30}},
		{"设置到期时间", "POST", "/api/users/%[1]d/expiry", map[string]any{"expires_at": "2099-01-01"}},
		{"授权节点", "POST", "/api/users/%[1]d/grants", map[string]any{"node_id": "NODE", "max_forwards": 5}},
		{"设置授权流量配额", "POST", "/api/users/%[1]d/nodes/%[2]d/quota", map[string]any{"traffic_quota_bytes": 1 << 30}},
		{"设置授权限速", "POST", "/api/users/%[1]d/nodes/%[2]d/rate-limit", map[string]any{"rate_limit_mbytes": 10}},
		{"重置授权流量", "POST", "/api/users/%[1]d/nodes/%[2]d/reset-traffic", nil},
		{"启用/停用账号", "POST", "/api/users/%[1]d/toggle", nil},
		{"修改用户资料", "PATCH", "/api/users/%[1]d/profile", map[string]any{"max_forwards": 10}},
		{"节点启停", "POST", "/api/nodes/%[2]d/toggle", nil},
		{"设置中继地址", "POST", "/api/nodes/%[2]d/relay-host", map[string]any{"relay_host": "2.2.2.2"}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			for _, lost := range []bool{false, true} {
				d := openDB(t)
				admin := loginAsAdmin(t, d)
				uid, _ := loginAsUser(t, d, 10)
				node := grantedNode(t, d, "edge", uid, 5)
				s, _ := New(d)
				body := map[string]any{}
				for k, v := range c.body {
					if v == "NODE" {
						v = node.ID
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
