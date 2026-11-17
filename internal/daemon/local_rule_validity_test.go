package daemon

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

// 本地规则合不合法由 daemon 在收到请求时裁决（nft.Validate），TUI 只做表单层面的
// 粗检。不合法的请求回 400 并说明原因，本地名下与数据面都不动。
var invalidLocalRules = []struct {
	what, body, reason string
}{
	{"UDP 配用户态转发", `{"proto":"udp","exit_host":"1.2.3.4","exit_port":80,"listen_port":12000,"mode":"userspace"}`, "UDP 不支持用户态转发"},
	{"目标端口为 0", `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":0,"listen_port":12000}`, "目标端口"},
	{"目标端口超出范围", `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":70000,"listen_port":12000}`, "目标端口"},
	{"监听端口超出范围", `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":80,"listen_port":70000}`, "监听端口"},
	{"不认识的协议", `{"proto":"icmp","exit_host":"1.2.3.4","exit_port":80,"listen_port":12000}`, "协议"},
	{"不认识的转发模式", `{"proto":"tcp","exit_host":"1.2.3.4","exit_port":80,"listen_port":12000,"mode":"bogus"}`, "转发模式"},
	{"目标既不是 IP 也不是域名", `{"proto":"tcp","exit_host":"bad host!","exit_port":80,"listen_port":12000}`, "目标"},
	{"目标像地址但不可能解析", `{"proto":"tcp","exit_host":"1.2.3.999","exit_port":80,"listen_port":12000}`, "目标"},
}

func TestStandaloneDaemonRefusesAnInvalidLocalRule(t *testing.T) {
	for _, c := range invalidLocalRules {
		dp := &fakeDataplane{}
		d := newTestDaemon(t)
		d.dp = dp

		w := serve(d, http.MethodPost, "/v1/rules", c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s：新建应回 400，实际 %d %s", c.what, w.Code, strings.TrimSpace(w.Body.String()))
			continue
		}
		if !strings.Contains(w.Body.String(), c.reason) {
			t.Errorf("%s：应说明原因（含“%s”），实际 %q", c.what, c.reason, strings.TrimSpace(w.Body.String()))
		}
		if n := len(d.owners[ownerLocal]); n != 0 {
			t.Errorf("%s：被拒绝的新建不应落到本地名下，实际 %d 条", c.what, n)
		}
		if n := len(dp.nftCalls); n != 0 {
			t.Errorf("%s：被拒绝的新建不应动数据面，实际下发 %d 次", c.what, n)
		}
	}

	// 对照：合法的请求照常建成。
	d := newTestDaemon(t)
	d.dp = &fakeDataplane{}
	if w := serve(d, http.MethodPost, "/v1/rules", createBody); w.Code != http.StatusOK {
		t.Fatalf("合法的新建应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if n := len(d.owners[ownerLocal]); n != 1 {
		t.Fatalf("合法的新建应落到本地名下，实际 %d 条", n)
	}
}

// 改本地规则时裁决的是改完之后的整条规则：单看请求里的每个字段都合法，合起来不
// 合法的（把一条 UDP 规则改成用户态转发）同样被拒，原规则原样保留。
func TestStandaloneDaemonRefusesAnEditThatMakesALocalRuleInvalid(t *testing.T) {
	edits := []struct {
		what, body, reason string
	}{
		{"UDP 规则改成用户态转发", `{"mode":"userspace"}`, "UDP 不支持用户态转发"},
		{"目标端口超出范围", `{"exit_port":70000}`, "目标端口"},
		{"监听端口超出范围", `{"listen_port":70000}`, "监听端口"},
		{"不认识的协议", `{"proto":"icmp"}`, "协议"},
		{"目标既不是 IP 也不是域名", `{"exit_host":"bad host!"}`, "目标"},
	}
	// 先经本机控制口建一条合法的 UDP 规则，让它落到本地名下与数据面，再改它。
	seeded := func() (*Daemon, *fakeDataplane, nft.Rule) {
		t.Helper()
		dp := &fakeDataplane{}
		d := newTestDaemon(t)
		d.dp = dp
		w := serve(d, http.MethodPost, "/v1/rules", `{"proto":"udp","exit_host":"1.2.3.4","exit_port":53,"listen_port":12000,"comment":"dns"}`)
		if w.Code != http.StatusOK || len(d.owners[ownerLocal]) != 1 {
			t.Fatalf("前置条件：应建成一条本地规则，实际 %d %s，本地名下 %d 条", w.Code, w.Body.String(), len(d.owners[ownerLocal]))
		}
		return d, dp, d.owners[ownerLocal][0]
	}
	for _, c := range edits {
		d, dp, before := seeded()
		applied := len(dp.nftCalls)

		w := serve(d, http.MethodPut, "/v1/rules/local/"+before.ID, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s：修改应回 400，实际 %d %s", c.what, w.Code, strings.TrimSpace(w.Body.String()))
		} else if !strings.Contains(w.Body.String(), c.reason) {
			t.Errorf("%s：应说明原因（含“%s”），实际 %q", c.what, c.reason, strings.TrimSpace(w.Body.String()))
		}
		if got := d.owners[ownerLocal]; len(got) != 1 || got[0] != before {
			t.Errorf("%s：被拒绝的修改不应改动原规则，实际 %s", c.what, fmt.Sprintf("%+v", got))
		}
		if n := len(dp.nftCalls) - applied; n != 0 {
			t.Errorf("%s：被拒绝的修改不应动数据面，实际又下发了 %d 次", c.what, n)
		}
	}

	// 对照：合法的修改照常生效。
	d, _, before := seeded()
	if w := serve(d, http.MethodPut, "/v1/rules/local/"+before.ID, `{"exit_port":5353,"comment":"mdns"}`); w.Code != http.StatusOK {
		t.Fatalf("合法的修改应成功，实际 %d %s", w.Code, w.Body.String())
	}
	if got := d.owners[ownerLocal][0]; got.DestPort != 5353 || got.Comment != "mdns" {
		t.Fatalf("合法的修改应生效，实际 %+v", got)
	}
}
