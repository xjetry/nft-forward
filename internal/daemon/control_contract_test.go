package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/wsproto"
)

// 本机控制口的两端——daemon 的处理函数与 daemonclient——各自定义了一份应答的形状。
// 这个文件里的测试让真实的客户端去读真实的处理函数，把两份定义钉在一起：任何
// 一边单独改了形态的名字或地址的形状，这里会红。

// 形态的名字在两边逐字相同，“是否托管”的判定也一致。
func TestStatusModeNamesAgreeBetweenDaemonAndClient(t *testing.T) {
	cases := []struct {
		mode RunMode
		want daemonclient.Mode
	}{
		{Standalone(), daemonclient.ModeStandalone},
		{RemotePanel("wss://panel.example/v1/agents", "tok"), daemonclient.ModeRemotePanel},
		{LocalPanel(""), daemonclient.ModeLocalPanel},
	}
	for _, tc := range cases {
		d, srv := newTestServer(t, &fakeDataplane{})
		d.mode = tc.mode
		c, err := daemonclient.New(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		st, err := c.Status()
		srv.Close()
		if err != nil {
			t.Errorf("%s：客户端读不了 daemon 报的状态: %v", tc.mode.Name(), err)
			continue
		}
		if st.Mode != tc.want {
			t.Errorf("%s：客户端读到的形态是 %q，应为 %q", tc.mode.Name(), st.Mode, tc.want)
		}
		if st.Mode.Managed() != tc.mode.Managed() {
			t.Errorf("%s：两边对“是否托管”的判定不一致（daemon %v，客户端 %v）", tc.mode.Name(), tc.mode.Managed(), st.Mode.Managed())
		}
	}
}

// 托管且在线的 daemon 上，本地名下与面板名下各有一条规则，两条规则的 id 取值相同。
// 客户端从列表拿到每一行的地址、原样交回，请求必须回到这一行的归属上：改删本地
// 那一行只动本地名下的规则，改删面板那一行只变成发给面板的命令。新建不看列表，同样由
// 面板裁决。
func TestListedAddressesRouteBackToTheOwnerTheyCameFrom(t *testing.T) {
	fh := newFakeHub()
	fh.onAck(wsproto.TypeHello, func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.HelloAck{NodeID: 7, Name: "edge"})
		return wsproto.Envelope{Type: wsproto.TypeHelloAck, ID: env.ID, Payload: ack}
	})
	ok := func(env wsproto.Envelope) wsproto.Envelope {
		ack, _ := json.Marshal(wsproto.RuleCmdAck{OK: true, Entry: "203.0.113.7:12000"})
		return wsproto.Envelope{Type: wsproto.TypeRuleCmdAck, ID: env.ID, Payload: ack}
	}
	fh.onAck(wsproto.TypeRuleUpdate, ok)
	fh.onAck(wsproto.TypeRuleDelete, ok)
	fh.onAck(wsproto.TypeRuleCreate, ok)
	hubSrv := httptest.NewServer(fh.handler(t))
	defer hubSrv.Close()

	d, srv := newTestServer(t, &fakeDataplane{})
	defer srv.Close()
	panel := RemotePanel("ws"+strings.TrimPrefix(hubSrv.URL, "http")+"/", "tok")
	d.mode = panel
	d.owners = OwnerRuleset{
		"panel": {{RuleID: 12345678, HopCount: 1, Proto: "tcp", SrcPort: 90, DestIP: "2.0.0.0", DestPort: 90}},
		"tui":   {{ID: "12345678", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80, Comment: "orig"}},
	}
	dl := NewDialer(DialerConfig{
		Panel: panel, AgentVersion: "v1",
		// 本地名下不交给假面板接管：这里要看的是地址的路由，接管另有测试。
		GetState: func() (OwnerRuleset, AgentMeta) { return OwnerRuleset{}, AgentMeta{} },
		OnApply:  func(_ context.Context, rev string, rules []nft.Rule) (string, error) { return "", nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = dl.runOnce(ctx) }()
	waitConnected(t, dl)
	d.dialer.Store(dl)

	c, err := daemonclient.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rows) != 2 || rows[0].SrcPort != 90 || rows[1].SrcPort != 80 {
		t.Fatalf("应列出面板规则（90）与本地规则（80），实际 %+v", rows)
	}
	panelRow, localRow := rows[0], rows[1]
	if panelRow.Ref.Local() || !localRow.Ref.Local() {
		t.Fatalf("面板那一行不应是本地地址、本地那一行应是本地地址，实际 %v / %v", panelRow.Ref, localRow.Ref)
	}

	localRules := func() []nft.Rule {
		d.mu.Lock()
		defer d.mu.Unlock()
		return append([]nft.Rule(nil), d.owners["tui"]...)
	}
	framesOf := func(typ string) []wsproto.Envelope {
		var out []wsproto.Envelope
		for _, f := range fh.Frames() {
			if f.Type == typ {
				out = append(out, f)
			}
		}
		return out
	}

	// 面板那一行：改与删都变成发给面板的命令，带的是面板规则 id；本地名下不动。
	if err := c.UpdateRule(panelRow.Ref, daemonclient.UpdateRuleReq{Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, Comment: "x"}); err != nil {
		t.Fatalf("改面板那一行: %v", err)
	}
	updates := framesOf(wsproto.TypeRuleUpdate)
	if len(updates) != 1 {
		t.Fatalf("改面板那一行应向面板发一条 rule_update，实际 %d 条", len(updates))
	}
	var upd wsproto.RuleUpdate
	if err := json.Unmarshal(updates[0].Payload, &upd); err != nil || upd.RuleID != 12345678 {
		t.Fatalf("rule_update 应指向面板规则 12345678，实际 %+v（%v）", upd, err)
	}
	if err := c.DeleteRule(panelRow.Ref); err != nil {
		t.Fatalf("删面板那一行: %v", err)
	}
	deletes := framesOf(wsproto.TypeRuleDelete)
	if len(deletes) != 1 {
		t.Fatalf("删面板那一行应向面板发一条 rule_delete，实际 %d 条", len(deletes))
	}
	var del wsproto.RuleDelete
	if err := json.Unmarshal(deletes[0].Payload, &del); err != nil || del.RuleID != 12345678 {
		t.Fatalf("rule_delete 应指向面板规则 12345678，实际 %+v（%v）", del, err)
	}
	if left := localRules(); len(left) != 1 || left[0].Comment != "orig" {
		t.Fatalf("面板那一行的改删不应动本地名下的规则，实际 %+v", left)
	}

	// 新建：托管形态下交给面板，本地名下不多出规则。
	if _, err := c.CreateRule(daemonclient.CreateRuleReq{Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443, ListenPort: 12000}); err != nil {
		t.Fatalf("新建: %v", err)
	}
	if n := len(framesOf(wsproto.TypeRuleCreate)); n != 1 {
		t.Fatalf("托管形态下新建应向面板发一条 rule_create，实际 %d 条", n)
	}
	if left := localRules(); len(left) != 1 {
		t.Fatalf("托管形态下新建不应落到本地名下，实际 %+v", left)
	}

	// 本地那一行：改与删只动本地名下的规则，不向面板发任何规则命令。
	if err := c.UpdateRule(localRow.Ref, daemonclient.UpdateRuleReq{Comment: "edited"}); err != nil {
		t.Fatalf("改本地那一行: %v", err)
	}
	if left := localRules(); len(left) != 1 || left[0].Comment != "edited" {
		t.Fatalf("本地那一行应被改掉，实际 %+v", left)
	}
	if err := c.DeleteRule(localRow.Ref); err != nil {
		t.Fatalf("删本地那一行: %v", err)
	}
	if left := localRules(); len(left) != 0 {
		t.Fatalf("本地那一行应被删掉，实际 %+v", left)
	}
	if u, dd := len(framesOf(wsproto.TypeRuleUpdate)), len(framesOf(wsproto.TypeRuleDelete)); u != 1 || dd != 1 {
		t.Fatalf("本地那一行的改删不应向面板发命令，rule_update %d 条、rule_delete %d 条（各应仍为 1）", u, dd)
	}
}
