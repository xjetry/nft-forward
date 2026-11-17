package daemon

import (
	"context"
	"errors"
	"testing"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// dnsStub 是一个可切换成败、可改答案的 DNS 查询，经真实的 nft.ResolveHosts 接入
// daemon：这里要钉的正是“解析失败时 ResolveHosts 的输出”与 reconcileOwners 之间的
// 配合，不能用直接返回规则的假 resolveFn 绕过去。
type dnsStub struct {
	fail    bool
	answers map[string]string
}

func (s *dnsStub) install(d *Daemon) {
	d.resolveFn = defaultResolver(&resolver.Resolver{Lookup: func(_ context.Context, host string) ([]string, error) {
		if s.fail {
			return nil, errors.New("lookup " + host + ": i/o timeout")
		}
		ip, ok := s.answers[host]
		if !ok {
			return nil, errors.New("lookup " + host + ": no such host")
		}
		return []string{ip}, nil
	}})
}

// liveTarget 返回数据面最近一次收到的规则集里，监听端口为 port 的那条规则的目标
// 地址；该规则不在其中时 ok 为 false。
func liveTarget(dp *fakeDataplane, port int) (ip string, ok bool) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if len(dp.nftCalls) == 0 {
		return "", false
	}
	for _, r := range dp.nftCalls[len(dp.nftCalls)-1] {
		if r.SrcPort == port {
			return r.DestIP, true
		}
	}
	return "", false
}

func applyCount(dp *fakeDataplane) int {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	return len(dp.nftCalls)
}

// 已经在转发的域名规则，不因为一轮 DNS 解析失败而被摘出数据面：失败的那一项沿用
// 上一次解析成功、此刻正在数据面里生效的地址，解析恢复后再跟上新的答案。
func TestDNSFailureKeepsLiveHostnameTarget(t *testing.T) {
	dp := &fakeDataplane{}
	d, srv := newTestServer(t, dp)
	defer srv.Close()
	dns := &dnsStub{answers: map[string]string{"a.example.test": "203.0.113.7"}}
	dns.install(d)
	ctx := context.Background()

	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 8080, DestHost: "a.example.test", DestPort: 443},
		{Proto: "tcp", SrcPort: 8081, DestIP: "198.51.100.1", DestPort: 443},
	}
	if _, _, _, err := d.reconcileOwners(ctx, func(c OwnerRuleset) { c["panel"] = rules }, nil, true); err != nil {
		t.Fatal(err)
	}
	if ip, ok := liveTarget(dp, 8080); !ok || ip != "203.0.113.7" {
		t.Fatalf("前置条件：域名规则应已带着解析结果进入数据面，实际 ip=%q ok=%v", ip, ok)
	}
	applied := applyCount(dp)

	dns.fail = true
	if err := d.refreshOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ip, ok := liveTarget(dp, 8080); !ok || ip != "203.0.113.7" {
		t.Fatalf("DNS 解析失败后域名规则被摘出数据面或丢了地址：ip=%q ok=%v", ip, ok)
	}
	if got := applyCount(dp); got != applied {
		t.Fatalf("DNS 解析失败不应触发数据面重放：apply 次数 %d → %d", applied, got)
	}
	// 沿用的地址只存在于解析结果里：规则集（也是落盘的内容）仍是规则的原样。
	d.mu.Lock()
	stored := d.owners["panel"][0]
	d.mu.Unlock()
	if stored.DestHost != "a.example.test" || stored.DestIP != "" {
		t.Fatalf("解析出的地址被写回了规则集: %+v", stored)
	}

	// 解析恢复且答案变了：跟上新地址。
	dns.fail = false
	dns.answers["a.example.test"] = "203.0.113.9"
	if err := d.refreshOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ip, ok := liveTarget(dp, 8080); !ok || ip != "203.0.113.9" {
		t.Fatalf("解析恢复后应换成新地址：ip=%q ok=%v", ip, ok)
	}
}

// 反方向：沿用的只能是“同一个域名”上一次解析出的地址。规则的目标改成另一个域名、
// 而新域名解析不出来时，不能带着旧域名的地址继续转发——那会把流量送到用户已经
// 改掉的目标上。这条规则此时应被扣下，等新域名解析成功。
func TestDNSFailureDoesNotCarryAddressAcrossHostnames(t *testing.T) {
	dp := &fakeDataplane{}
	d, srv := newTestServer(t, dp)
	defer srv.Close()
	dns := &dnsStub{answers: map[string]string{"a.example.test": "203.0.113.7"}}
	dns.install(d)
	ctx := context.Background()

	if _, _, _, err := d.reconcileOwners(ctx, func(c OwnerRuleset) {
		c["panel"] = []nft.Rule{{Proto: "tcp", SrcPort: 8080, DestHost: "a.example.test", DestPort: 443}}
	}, nil, true); err != nil {
		t.Fatal(err)
	}
	if ip, ok := liveTarget(dp, 8080); !ok || ip != "203.0.113.7" {
		t.Fatalf("前置条件不成立：ip=%q ok=%v", ip, ok)
	}

	// b.example.test 没有答案：解析失败。
	_, unresolved, _, err := d.reconcileOwners(ctx, func(c OwnerRuleset) {
		c["panel"] = []nft.Rule{{Proto: "tcp", SrcPort: 8080, DestHost: "b.example.test", DestPort: 443}}
	}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].DestHost != "b.example.test" {
		t.Fatalf("改到解析不出的新域名后，规则应被扣下：unresolved=%+v", unresolved)
	}
	if ip, ok := liveTarget(dp, 8080); ok {
		t.Fatalf("规则不应带着旧域名的地址留在数据面：ip=%q", ip)
	}
}
