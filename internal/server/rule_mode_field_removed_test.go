package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// 规则请求体的顶层字段 mode 已经删除，出口那一跳的转发模式只用 exit_mode 给。带着
// mode 的请求在六个写规则的入口上都回 400 并说明该用哪个字段，不建规则，也不改
// 已有的规则——悄悄忽略它的话，仍在发 mode 的客户端会以为自己指定了出口模式。
// hops 里每一跳的 mode 是另一个字段，不受影响。
func TestRuleRequestsCarryingTheRemovedModeFieldAreRefused(t *testing.T) {
	d := openDB(t)
	uid, userCookie := loginAsUser(t, d, 10)
	tok, err := db.CreateAPIToken(d, uid, db.TokenScopeReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	n := grantedNode(t, d, "n1", uid, 10)
	admin := loginAsAdmin(t, d)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}

	// 一条已有的规则，属主是这个用户，出口模式 userspace：改它的请求被拒绝后必须原样。
	existing := adminCreateRuleID(t, s, admin, map[string]any{
		"node_id": n.ID, "name": "existing", "proto": "tcp", "exit": "9.9.9.9:443", "exit_mode": "userspace",
	})
	if _, err := d.Exec(`UPDATE rules SET owner_id=? WHERE id=?`, uid, existing); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var rules int
		if err := d.QueryRow(`SELECT COUNT(*) FROM rules`).Scan(&rules); err != nil {
			t.Fatal(err)
		}
		rl, err := db.GetRule(d, existing)
		if err != nil {
			t.Fatal(err)
		}
		hops, err := db.ListRuleHops(d, existing)
		if err != nil {
			t.Fatal(err)
		}
		modes := make([]string, len(hops))
		for i, h := range hops {
			modes[i] = h.Mode
		}
		return fmt.Sprintf("规则 %d 条；%s 的名字 %q、各跳模式 %v", rules, "existing", rl.Name, modes)
	}
	before := snapshot()

	create := func(mode any) map[string]any {
		return map[string]any{"node_id": n.ID, "name": "fresh", "proto": "tcp", "exit": "9.9.9.9:443", "mode": mode}
	}
	update := func(mode any) map[string]any {
		return map[string]any{"name": "renamed", "proto": "tcp", "exit": "9.9.9.9:443", "mode": mode}
	}
	rulePath := fmt.Sprintf("/rules/%d", existing)
	type call struct {
		name string
		do   func(mode any) (int, string)
	}
	session := func(cookie *http.Cookie, method, path string, body func(any) map[string]any) func(any) (int, string) {
		return func(mode any) (int, string) {
			rec := adminRequest(t, s, cookie, method, path, body(mode))
			return rec.Code, rec.Body.String()
		}
	}
	token := func(method, path string, body func(any) map[string]any) func(any) (int, string) {
		return func(mode any) (int, string) {
			rec := v1Do(t, s, method, path, tok, body(mode))
			return rec.Code, rec.Body.String()
		}
	}
	calls := []call{
		{"管理员建规则", session(admin, "POST", "/api/rules", create)},
		{"管理员改规则", session(admin, "PUT", "/api"+rulePath, update)},
		{"用户建规则", session(userCookie, "POST", "/api/my/rules", create)},
		{"用户改规则", session(userCookie, "PUT", "/api/my"+rulePath, update)},
		{"令牌建规则", token("POST", "/api/v1/my/rules", create)},
		{"令牌改规则", token("PUT", "/api/v1/my"+rulePath, update)},
	}
	// 字段只要出现就拒绝，不看它的值：空串与 null 也是“带了这个字段”。
	for _, mode := range []any{"kernel", "userspace", "", nil} {
		for _, c := range calls {
			code, body := c.do(mode)
			if code != http.StatusBadRequest || !strings.Contains(body, "mode") || !strings.Contains(body, "exit_mode") {
				t.Errorf("%s，mode=%#v：回了 %d %s，应回 400 并说明改用 exit_mode", c.name, mode, code, body)
			}
			if after := snapshot(); after != before {
				t.Errorf("%s，mode=%#v：被拒绝的请求改动了数据\n之前：%s\n之后：%s", c.name, mode, before, after)
			}
		}
	}

	// 不带顶层 mode 的请求照常：exit_mode 指定出口，hops 里每一跳自己的 mode 不算。
	rec := adminRequest(t, s, admin, "PUT", "/api"+rulePath, map[string]any{
		"name": "existing", "proto": "tcp", "exit": "9.9.9.9:443", "exit_mode": "kernel",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("只带 exit_mode 的改规则应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = adminRequest(t, s, admin, "POST", "/api/rules", map[string]any{
		"name": "explicit", "proto": "tcp", "exit": "9.9.9.9:443",
		"hops": []map[string]any{{"node_id": n.ID, "mode": "userspace"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("hops 里每一跳的 mode 不是被删除的字段，请求应成功，实际 %d %s", rec.Code, rec.Body.String())
	}
}
