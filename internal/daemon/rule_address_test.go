package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
)

// 规则地址的命名空间由路径写明。这里经真实的 daemonclient 走到真实的 Handler，
// 把“客户端拼出的地址”与“daemon 的分发”钉在一起：两边各自的单测都只看得见自己
// 那一半，任何一边单独改了路径形状，这里会红。
func TestRuleAddressLocalNamespaceIgnoresIDShape(t *testing.T) {
	// 单机模式（没有 dialer）。本地规则的 ID 全是数字：nft.NewRuleID 生成 8 位
	// 十六进制，这种取值是合法的。
	d, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	d.owners = OwnerRuleset{"tui": {
		{ID: "12345678", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
		{ID: "abcd1234", Proto: "tcp", SrcPort: 81, DestIP: "1.0.0.0", DestPort: 81},
	}}
	c, err := daemonclient.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.UpdateRule(daemonclient.LocalRule("12345678"), daemonclient.UpdateRuleReq{Comment: "updated"}); err != nil {
		t.Fatalf("改全数字 id 的本地规则: %v", err)
	}
	d.mu.Lock()
	got := d.owners["tui"][0].Comment
	d.mu.Unlock()
	if got != "updated" {
		t.Fatalf("本地规则未被修改: comment=%q", got)
	}

	if err := c.DeleteRule(daemonclient.LocalRule("12345678")); err != nil {
		t.Fatalf("删全数字 id 的本地规则: %v", err)
	}
	d.mu.Lock()
	left := append([]nft.Rule(nil), d.owners["tui"]...)
	d.mu.Unlock()
	if len(left) != 1 || left[0].ID != "abcd1234" {
		t.Fatalf("应只剩 abcd1234，实际 %+v", left)
	}
}

// 反方向：panel 命名空间的请求永远走面板通道。同一串数字在本地名下存在同名的
// 本地规则时，也不得落到本地规则上。
func TestRuleAddressPanelNamespaceNeverTouchesLocalRules(t *testing.T) {
	// 面板通道在两种情形下都用不了：单机形态没有面板，托管形态此刻没有会话。请求
	// 必须各以自己的原因失败，而不是改到本地规则上。
	cases := []struct {
		name string
		mode RunMode
		want string
	}{
		{"单机形态", Standalone(), "没有面板规则"},
		{"托管但离线", RemotePanel("wss://panel.example/v1/agents", "tok"), "面板不可达"},
	}
	for _, tc := range cases {
		d, srv := newTestServer(t, &fakeDataplane{})
		d.mode = tc.mode
		d.owners = OwnerRuleset{"tui": {
			{ID: "12345678", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80, Comment: "orig"},
		}}
		c, err := daemonclient.New(srv.URL)
		if err != nil {
			t.Fatal(err)
		}

		err = c.UpdateRule(daemonclient.PanelRule(12345678), daemonclient.UpdateRuleReq{Comment: "x"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：改 panel 命名空间应报 %q，实际 %v", tc.name, tc.want, err)
		}
		err = c.DeleteRule(daemonclient.PanelRule(12345678))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：删 panel 命名空间应报 %q，实际 %v", tc.name, tc.want, err)
		}
		d.mu.Lock()
		rules := append([]nft.Rule(nil), d.owners["tui"]...)
		d.mu.Unlock()
		if len(rules) != 1 || rules[0].Comment != "orig" {
			t.Fatalf("%s：本地规则不应被 panel 命名空间的请求改动: %+v", tc.name, rules)
		}
		srv.Close()
	}
}

// 不带命名空间的地址、未知命名空间、非正整数的面板 id 都被拒绝，且不落到任何规则上。
func TestRuleAddressRejectsMalformed(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"/v1/rules/abcd1234", http.StatusNotFound},
		{"/v1/rules/12345678", http.StatusNotFound},
		{"/v1/rules/other/abcd1234", http.StatusNotFound},
		{"/v1/rules/local/", http.StatusNotFound},
		{"/v1/rules/panel/abcd", http.StatusBadRequest},
		{"/v1/rules/panel/0", http.StatusBadRequest},
		{"/v1/rules/panel/-3", http.StatusBadRequest},
	}
	for _, tc := range cases {
		d := newTestDaemon(t)
		d.owners = OwnerRuleset{"tui": {
			{ID: "abcd1234", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80, Comment: "orig"},
			{ID: "12345678", Proto: "tcp", SrcPort: 81, DestIP: "1.0.0.0", DestPort: 81, Comment: "orig"},
		}}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, tc.path, strings.NewReader(`{"comment":"x"}`))
			w := httptest.NewRecorder()
			d.Handler().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s %s: status=%d, want %d (%s)", method, tc.path, w.Code, tc.want, strings.TrimSpace(w.Body.String()))
			}
		}
		if len(d.owners["tui"]) != 2 || d.owners["tui"][0].Comment != "orig" || d.owners["tui"][1].Comment != "orig" {
			t.Errorf("%s: 被拒绝的请求改动了本地规则: %+v", tc.path, d.owners["tui"])
		}
	}
}
